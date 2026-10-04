package aznet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/google/uuid"
)

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

func TestQueueDeletionRetainsReceiptOnFailure(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			w.WriteHeader(201)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if r.URL.Query().Get("popreceipt") != "receipt" {
			t.Error("lost receipt")
		}
		if attempts == 1 {
			sdkFailure(w, "azqueue", 403, "AuthorizationFailure")
			return
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL + "/account")
	u.User = url.UserPassword("account", "dGVzdC1rZXk=")
	cfg := applyConfig(nil)
	defer cfg.cancel()
	driver, err := (&queueFactory{}).NewDriver(NewEndpoint(u), cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := driver.(*queueDriver)
	d.receipts.Store("session", "message:receipt")
	if err := d.DeleteToken(context.Background(), "session"); err == nil {
		t.Fatal("hidden deletion failure")
	}
	if _, ok := d.receipts.Load("session"); !ok {
		t.Fatal("receipt discarded on failure")
	}
	if err := d.DeleteToken(context.Background(), "session"); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.receipts.Load("session"); ok {
		t.Fatal("receipt retained after success")
	}
}
