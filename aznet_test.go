package aznet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/google/uuid"
)

// Exercise the real SDK response decoders and bootstrap creation paths without
// Azure credentials or a live service. Request counts also guard against retries.
func TestBootstrapResponseClassification(t *testing.T) {
	for _, backend := range []struct {
		network, resource, deleting, exists string
	}{
		{"azblob", "container", "ContainerBeingDeleted", "ContainerAlreadyExists"},
		{"azqueue", "queue", "QueueBeingDeleted", "QueueAlreadyExists"},
		{"aztable", "table", "TableBeingDeleted", "TableAlreadyExists"},
	} {
		t.Run(backend.network, func(t *testing.T) {
			for _, tc := range []struct {
				name, code                string
				status                    int
				wantDeleting, wantSuccess bool
			}{
				{"created", "", http.StatusCreated, false, true},
				{"deleting", backend.deleting, http.StatusConflict, true, false},
				{"already_exists", backend.exists, http.StatusConflict, false, true},
				{"unrelated_conflict", "LeaseIdMissing", http.StatusConflict, false, false},
				{"forbidden", "AuthorizationFailure", http.StatusForbidden, false, false},
			} {
				for _, target := range []string{"handshake", "token"} {
					t.Run(tc.name+"/"+target, func(t *testing.T) {
						var requests atomic.Int32
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							n := requests.Add(1)
							name := strings.TrimPrefix(r.URL.Path, "/testaccount/")
							method := http.MethodPut
							if backend.network == "aztable" {
								method = http.MethodPost
								var entity struct{ TableName string }
								if err := json.NewDecoder(r.Body).Decode(&entity); err != nil {
									t.Errorf("decode table request: %v", err)
								}
								name = entity.TableName
								if r.URL.Path != "/testaccount/Tables" {
									t.Errorf("unexpected table path: %s", r.URL.Path)
								}
							}
							wantName := "handshake"
							if n == 2 {
								wantName = "token"
							}
							if n > 2 || name != wantName || r.Method != method {
								t.Errorf("request %d: %s %s (resource %q)", n, r.Method, r.URL, name)
							}
							if name == target && tc.code != "" {
								w.Header().Set("x-ms-error-code", tc.code)
								w.Header().Set("x-ms-request-id", "classification-test")
								if backend.network == "aztable" {
									w.Header().Set("Content-Type", "application/json")
									w.WriteHeader(tc.status)
									fmt.Fprintf(w, `{"odata.error":{"code":%q,"message":{"lang":"en-US","value":"injected failure"}}}`, tc.code)
								} else {
									w.Header().Set("Content-Type", "application/xml")
									w.WriteHeader(tc.status)
									fmt.Fprintf(w, `<Error><Code>%s</Code><Message>injected failure</Message></Error>`, tc.code)
								}
								return
							}
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusCreated)
							if backend.network == "aztable" {
								fmt.Fprintf(w, `{"TableName":%q}`, name)
							}
						}))
						defer server.Close()
						u, err := url.Parse(server.URL + "/testaccount")
						if err != nil {
							t.Fatal(err)
						}
						u.User = url.UserPassword("testaccount", "dGVzdC1rZXk=")
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						opts := []Option{WithContext(ctx), WithEndpoints("handshake", "token")}
						if tc.wantSuccess {
							// Initialization covers both resources without starting a janitor.
							driver, _, _, err := initialize(backend.network, u.String(), opts)
							if err != nil || driver == nil {
								t.Fatalf("initialize: driver=%v, error=%v", driver, err)
							}
						} else {
							listener, err := Listen(backend.network, u.String(), opts...)
							if listener != nil || err == nil {
								t.Fatalf("Listen: listener=%v, error=%v", listener, err)
							}
							// Callers may add their own context without losing either error.
							for _, got := range []error{err, fmt.Errorf("caller: %w", err)} {
								if errors.Is(got, ErrResourceBeingDeleted) != tc.wantDeleting {
									t.Errorf("deletion classification: %v", got)
								}
								var sdkErr *azcore.ResponseError
								if !errors.As(got, &sdkErr) {
									t.Errorf("SDK error missing from chain: %v", got)
									continue
								}
								if !errors.Is(got, sdkErr) {
									t.Error("SDK error identity missing from chain")
								}
								if sdkErr.ErrorCode != tc.code || sdkErr.StatusCode != tc.status {
									t.Errorf("SDK error: %+v", sdkErr)
								}
								if sdkErr.RawResponse == nil || sdkErr.RawResponse.Header.Get("x-ms-request-id") != "classification-test" {
									t.Error("SDK raw response lost")
								}
							}
							if tc.wantDeleting && !strings.Contains(err.Error(), fmt.Sprintf("%s %q", backend.resource, target)) {
								t.Errorf("resource context missing: %v", err)
							}
						}
						wantRequests := int32(2)
						if !tc.wantSuccess && target == "handshake" {
							wantRequests = 1
						}
						if got := requests.Load(); got != wantRequests {
							t.Errorf("requests = %d, want %d (no retries or creation after failure)", got, wantRequests)
						}
					})
				}
			}
		})
	}
}

// Exercise actual SDK calls for every session acquisition, including Blob's
// append objects, and verify rollback targets session resources exclusively.
func TestAdapterAcquisitionRollback(t *testing.T) {
	for _, network := range []string{"azblob", "azqueue", "aztable"} {
		stages := 2
		if network == "azblob" {
			stages = 3
		}
		for fail := 1; fail <= stages; fail++ {
			t.Run(fmt.Sprintf("%s/acquisition%d", network, fail), func(t *testing.T) {
				var mu sync.Mutex
				acquired := 0
				var deleted []string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					if r.Method == http.MethodDelete {
						deleted = append(deleted, r.URL.Path)
						writeDeleteSuccess(w, network, r.URL.Path)
						return
					}
					// Bootstrap creation succeeds before the injected session failure.
					name := strings.TrimPrefix(r.URL.Path, "/account/")
					if network == "aztable" {
						var v struct{ TableName string }
						json.NewDecoder(r.Body).Decode(&v)
						name = v.TableName
					}
					if name != "handshake" && name != "token" {
						acquired++
						if acquired == fail {
							sdkFailure(w, network, 403, "AuthorizationFailure")
							return
						}
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(201)
					if network == "aztable" {
						fmt.Fprintf(w, `{"TableName":%q}`, name)
					}
				}))
				defer server.Close()
				u, _ := url.Parse(server.URL + "/account")
				u.User = url.UserPassword("account", "dGVzdC1rZXk=")
				cfg := applyConfig([]Option{WithPing(0)})
				defer cfg.cancel()
				factory, _ := lookupFactory(network)
				driver, err := factory.NewDriver(NewEndpoint(u), cfg)
				if err != nil {
					t.Fatal(err)
				}
				id := uuid.NewString()
				noise, _ := NewNoiseClient()
				msg, _ := noise.WriteMessage([]byte(id))
				hsID := id
				if network == "azqueue" {
					hsID += ":receipt"
				}
				l := &Listener{driver: driver, cfg: cfg, pending: []Handshake{{ID: hsID, Payload: msg}}}
				_, err = l.Accept()
				var response *azcore.ResponseError
				if !errors.As(err, &response) || response.StatusCode != 403 {
					t.Fatalf("lost acquisition error: %v", err)
				}
				l.Close()
				mu.Lock()
				defer mu.Unlock()
				for _, path := range deleted {
					if path == "/account/handshake" || path == "/account/token" || path == "/account/Tables('handshake')" || path == "/account/Tables('token')" {
						t.Fatalf("deleted bootstrap %s", path)
					}
				}
				want := 4 // handshake + token + two session resources
				if network == "azblob" {
					want = 3
				}
				// Queue never posted a token, so has no receipt to delete one.
				if network == "azqueue" {
					want = 3
				}
				if len(deleted) != want {
					t.Fatalf("deletes=%v want %d", deleted, want)
				}
			})
		}
	}
}

func sdkFailure(w http.ResponseWriter, network string, status int, code string) {
	w.Header().Set("x-ms-error-code", code)
	if network == "aztable" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"odata.error":{"code":%q,"message":{"lang":"en-US","value":"injected"}}}`, code)
	} else {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		fmt.Fprintf(w, `<Error><Code>%s</Code><Message>injected</Message></Error>`, code)
	}
}

func TestAdapterCleanupContract(t *testing.T) {
	for _, network := range []string{"azblob", "azqueue", "aztable"} {
		for _, status := range []int{204, 403, 404} {
			t.Run(fmt.Sprintf("%s/%d", network, status), func(t *testing.T) {
				var mu sync.Mutex
				deletes := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodDelete {
						mu.Lock()
						deletes++
						mu.Unlock()
						if status != 204 {
							sdkFailure(w, network, status, "Injected")
							return
						}
						writeDeleteSuccess(w, network, r.URL.Path)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(201)
					if network == "aztable" {
						fmt.Fprint(w, `{"TableName":"test"}`)
					}
				}))
				defer server.Close()
				u, _ := url.Parse(server.URL + "/account")
				u.User = url.UserPassword("account", "dGVzdC1rZXk=")
				cfg := applyConfig(nil)
				defer cfg.cancel()
				f, _ := lookupFactory(network)
				d, err := f.NewDriver(NewEndpoint(u), cfg)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				for _, err := range []error{d.CleanupSession(ctx, uuid.NewString()), d.CleanupBootstrap(ctx)} {
					if (err != nil) != (status == 403) {
						t.Fatalf("cleanup: %v", err)
					}
					if status == 403 {
						var r *azcore.ResponseError
						if !errors.As(err, &r) || r.StatusCode != 403 {
							t.Fatal(err)
						}
					}
				}
				want := 4
				if network == "azblob" {
					want = 3
				}
				mu.Lock()
				defer mu.Unlock()
				if deletes != want {
					t.Fatalf("did not try every owned resource: %d", deletes)
				}
			})
		}
	}
}

func writeDeleteSuccess(w http.ResponseWriter, network, path string) {
	if network == "azblob" && !strings.Contains(path, "/handshake/") && !strings.Contains(path, "/token/") {
		w.WriteHeader(202)
	} else {
		w.WriteHeader(204)
	}
}

// TestLiveBootstrapDeletionWindow is opt-in: AZNET_LIVE_CONFIG names a local
// ProxyBlob-format configuration containing Azure account credentials. It creates
// and deletes only UUID-named bootstrap resources, never configured endpoints.
func TestLiveBootstrapDeletionWindow(t *testing.T) {
	path := os.Getenv("AZNET_LIVE_CONFIG")
	if path == "" {
		t.Skip("set AZNET_LIVE_CONFIG to explicitly enable live Azure resource operations")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read live configuration")
	}
	var config struct {
		Listeners []struct {
			Driver  string `json:"driver"`
			Address string `json:"address"`
			Account string `json:"storage_account"`
			Key     string `json:"storage_account_key"`
		} `json:"listeners"`
	}
	if json.Unmarshal(raw, &config) != nil {
		t.Fatal("invalid live configuration")
	}
	for _, network := range []string{"azblob", "azqueue", "aztable"} {
		t.Run(network, func(t *testing.T) {
			var address string
			for _, c := range config.Listeners {
				u, err := url.Parse(c.Address)
				if err == nil && c.Driver == network && u.Scheme == "https" && strings.HasSuffix(u.Hostname(), ".core.windows.net") && c.Account != "" && c.Key != "" {
					u.User = url.UserPassword(c.Account, c.Key)
					u.RawQuery = ""
					u.Path = ""
					address = u.String()
					break
				}
			}
			if address == "" {
				t.Fatal("no live account-key configuration for driver")
			}
			prefix := "aznetreview" + strings.ReplaceAll(uuid.NewString(), "-", "")
			handshake, token := prefix+"h", prefix+"t"
			t.Logf("isolated bootstrap resources: %s, %s", handshake, token)
			describe := func(err error) string {
				var response *azcore.ResponseError
				if errors.As(err, &response) {
					return fmt.Sprintf("status=%d code=%s", response.StatusCode, response.ErrorCode)
				}
				if err == nil {
					return "success"
				}
				// SDK errors may contain credential-bearing request URLs. Never print them.
				return fmt.Sprintf("error type %T", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			listen := func() (net.Listener, error) {
				return Listen(network, address, WithContext(ctx), WithEndpoints(handshake, token))
			}
			// Independent cleanup also covers partially successful initialization.
			u, _ := url.Parse(address)
			ep := NewEndpoint(u)
			remove := func(ctx context.Context, name string) error {
				switch network {
				case "azblob":
					c, err := newBlobClient(ep, nil)
					if err != nil {
						return err
					}
					_, err = c.NewContainerClient(name).Delete(ctx, nil)
					return err
				case "azqueue":
					c, err := newQueueClient(ep, nil)
					if err != nil {
						return err
					}
					_, err = c.NewQueueClient(name).Delete(ctx, nil)
					return err
				default:
					c, err := newTableClient(ep, nil)
					if err != nil {
						return err
					}
					_, err = c.DeleteTable(ctx, name, nil)
					return err
				}
			}
			t.Cleanup(func() {
				cleanupCtx, done := context.WithTimeout(context.Background(), 30*time.Second)
				defer done()
				for _, name := range []string{handshake, token} {
					err := remove(cleanupCtx, name)
					var response *azcore.ResponseError
					if err != nil && !(errors.As(err, &response) && response.StatusCode == 404) {
						t.Errorf("cleanup %s: %s", name, describe(err))
					}
				}
			})
			listener, err := listen()
			if err != nil {
				t.Fatalf("initial Listen: %s", describe(err))
			}
			if err := listener.Close(); err != nil {
				t.Fatalf("initial Close: %s", describe(err))
			}
			if err := listener.(*Listener).CleanupBootstrap(ctx); err != nil {
				t.Fatalf("explicit bootstrap cleanup: %s", describe(err))
			}
			start := time.Now()
			deleting := 0
			for {
				listener, err = listen()
				if err == nil {
					if err := listener.Close(); err != nil {
						t.Errorf("final Close: %s", describe(err))
					}
					if deleting == 0 {
						t.Fatal("no deletion-window response observed; live classification gate not established")
					}
					t.Logf("name reuse succeeded after %s; %d deletion responses validated", time.Since(start).Round(time.Millisecond), deleting)
					break
				}
				wrapped := fmt.Errorf("caller: %w", err)
				var response *azcore.ResponseError
				wantCode := map[string]string{"azblob": "ContainerBeingDeleted", "azqueue": "QueueBeingDeleted", "aztable": "TableBeingDeleted"}[network]
				if !errors.Is(wrapped, ErrResourceBeingDeleted) || !errors.As(wrapped, &response) || response.StatusCode != 409 || response.ErrorCode != wantCode || response.RawResponse == nil || !errors.Is(wrapped, response) {
					t.Fatalf("restart classification: %s; sentinel=%t", describe(err), errors.Is(wrapped, ErrResourceBeingDeleted))
				}
				if !strings.Contains(err.Error(), handshake) && !strings.Contains(err.Error(), token) {
					t.Fatal("resource context missing")
				}
				deleting++
				if deleting == 1 {
					t.Logf("immediate restart: %s; sentinel and SDK chain preserved", describe(err))
				}
				select {
				case <-ctx.Done():
					t.Fatal("name reuse not observed within three minutes")
				case <-time.After(5 * time.Second):
				}
			}
		})
	}
}
