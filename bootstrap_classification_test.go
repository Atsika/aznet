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
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
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
