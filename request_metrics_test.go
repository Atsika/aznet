package aznet

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	azruntime "github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
)

func TestSDKRequestAttemptsIncludeRetriesAndFailures(t *testing.T) {
	for _, network := range []string{"azblob", "azqueue", "aztable"} {
		t.Run(network, func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if n == 1 {
					w.Header().Set("x-ms-retry-after-ms", "1")
					w.Header().Set("x-ms-error-code", "ServerBusy")
					w.WriteHeader(503)
					fmt.Fprint(w, `{"odata.error":{"code":"ServerBusy","message":{"value":"busy"}}}`)
					return
				}
				if n == 3 {
					w.Header().Set("x-ms-error-code", "AuthorizationFailure")
					w.WriteHeader(403)
					fmt.Fprint(w, `{"odata.error":{"code":"AuthorizationFailure","message":{"value":"denied"}}}`)
					return
				}
				w.WriteHeader(201)
				if network == "aztable" {
					fmt.Fprint(w, `{"TableName":"handshake"}`)
				}
			}))
			defer server.Close()
			u, _ := url.Parse(server.URL + "/testaccount")
			u.User = url.UserPassword("testaccount", "dGVzdC1rZXk=")
			metrics := NewDefaultMetrics()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			l, err := Listen(network, u.String(), WithContext(ctx), WithMetrics(metrics), WithEndpoints("handshake", "token"))
			if l != nil || err == nil {
				t.Fatalf("Listen = %v, %v; want failed second resource", l, err)
			}
			if got := requests.Load(); got != 3 {
				t.Fatalf("fixture requests=%d want3", got)
			}
			if got := metrics.GetWriteTransactionCount(); got != 3 {
				t.Fatalf("SDK write attempts=%d want3 (initial503, retry201, second resource403)", got)
			}
			counts := metrics.RequestCounts()
			var total, retried, failed int64
			for attempt, count := range counts {
				total += count
				if attempt.Retry {
					retried += count
				}
				if attempt.Failed {
					failed += count
				}
				if attempt.Driver != network {
					t.Errorf("driver label=%q", attempt.Driver)
				}
			}
			if total != 3 || retried != 1 || failed != 2 {
				t.Fatalf("attempt snapshot: total=%d retries=%d failures=%d: %v", total, retried, failed, counts)
			}
			clear(counts)
			if len(metrics.RequestCounts()) == 0 {
				t.Fatal("snapshot aliases collector state")
			}
			if got := metrics.GetBytesSent(); got != 0 {
				t.Fatalf("resource creation must not invent payload bytes: %d", got)
			}
		})
	}
}

func TestSDKMetricsEmptyPollPaginationAndCleanup(t *testing.T) {
	for _, network := range []string{"azblob", "azqueue", "aztable"} {
		t.Run(network, func(t *testing.T) {
			var gets atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					if network != "azblob" {
						w.WriteHeader(204)
					} else {
						w.WriteHeader(202)
					}
					return
				}
				if r.Method != http.MethodGet {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(201)
					if network == "aztable" {
						fmt.Fprint(w, `{"TableName":"fixture"}`)
					}
					return
				}
				n := gets.Add(1)
				switch network {
				case "azblob":
					w.Header().Set("Content-Type", "application/xml")
					marker := ""
					if n == 1 {
						marker = "next"
					}
					fmt.Fprintf(w, `<EnumerationResults><Blobs></Blobs><NextMarker>%s</NextMarker></EnumerationResults>`, marker)
				case "azqueue":
					w.Header().Set("Content-Type", "application/xml")
					fmt.Fprint(w, `<QueueMessagesList></QueueMessagesList>`)
				case "aztable":
					w.Header().Set("Content-Type", "application/json")
					if n == 1 {
						w.Header().Set("x-ms-continuation-NextPartitionKey", "next")
						w.Header().Set("x-ms-continuation-NextRowKey", "next")
					}
					fmt.Fprint(w, `{"value":[]}`)
				}
			}))
			defer server.Close()
			u, _ := url.Parse(server.URL + "/testaccount")
			u.User = url.UserPassword("testaccount", "dGVzdC1rZXk=")
			m := NewDefaultMetrics()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			driver, _, cfg, err := initialize(network, u.String(), []Option{WithContext(ctx), WithMetrics(m), WithEndpoints("messages", "messagestoken")})
			if err != nil {
				t.Fatal(err)
			}
			defer cfg.cancel()
			hs, err := driver.GetHandshakes(ctx)
			if err != nil || len(hs) != 0 {
				t.Fatalf("empty poll: %v %v", hs, err)
			}
			if err := driver.CleanupBootstrap(ctx); err != nil {
				t.Fatal(err)
			}
			wantList, wantRead := int64(2), int64(0)
			if network == "azqueue" {
				wantList, wantRead = 0, 1
			}
			if m.GetListTransactionCount() != wantList || m.GetReadTransactionCount() != wantRead || m.GetWriteTransactionCount() != 2 || m.GetDeleteTransactionCount() != 2 {
				t.Fatalf("counts list/read/write/delete = %d/%d/%d/%d", m.GetListTransactionCount(), m.GetReadTransactionCount(), m.GetWriteTransactionCount(), m.GetDeleteTransactionCount())
			}
			if m.GetBytesReceived() != 0 || m.GetBytesSent() != 0 {
				t.Fatal("empty polls/resources counted as payload")
			}
			if network == "azqueue" {
				counts := m.RequestCounts()
				for _, operation := range []string{"CreateQueue", "DeleteQueue"} {
					var found int64
					for attempt, count := range counts {
						if attempt.Operation == operation {
							found += count
						}
					}
					if found != 2 {
						t.Errorf("%s attempts=%d want2 for queues whose names begin with messages", operation, found)
					}
				}
			}
			var total int64
			for attempt, n := range m.RequestCounts() {
				total += n
				if attempt.Retry || attempt.Failed {
					t.Fatalf("unexpected retry/failure: %v", attempt)
				}
			}
			if total != 4+wantList+wantRead {
				t.Fatalf("attempt total=%d", total)
			}
		})
	}
}

func TestSDKMetricsSASBootstrapAndSessionPolls(t *testing.T) {
	t.Setenv("AZURE_STORAGE_ACCOUNT_KEY", "")
	for _, network := range []string{"azblob", "azqueue", "aztable"} {
		t.Run(network, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected method %s", r.Method)
				}
				switch network {
				case "azblob":
					w.Header().Set("Content-Type", "application/xml")
					if r.URL.Query().Get("comp") == "list" {
						fmt.Fprint(w, `<EnumerationResults><Blobs></Blobs><NextMarker></NextMarker></EnumerationResults>`)
					} else {
						w.Header().Set("x-ms-error-code", "InvalidRange")
						w.WriteHeader(416)
						fmt.Fprint(w, `<Error><Code>InvalidRange</Code></Error>`)
					}
				case "azqueue":
					w.Header().Set("Content-Type", "application/xml")
					fmt.Fprint(w, `<QueueMessagesList></QueueMessagesList>`)
				case "aztable":
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"value":[]}`)
				}
			}))
			defer server.Close()
			u, _ := url.Parse(server.URL + "/testaccount")
			token := "sv=2025-01-05&sig=fixture-secret"
			q := u.Query()
			q.Set("handshake", base64.URLEncoding.EncodeToString([]byte(token)))
			q.Set("token", base64.URLEncoding.EncodeToString([]byte(token)))
			u.RawQuery = q.Encode()
			m := NewDefaultMetrics()
			driver, _, cfg, err := initialize(network, u.String(), []Option{WithMetrics(m), WithEndpoints("handshake", "token")})
			if err != nil {
				t.Fatal(err)
			}
			defer cfg.cancel()
			if _, err := driver.GetHandshakes(context.Background()); err != nil {
				t.Fatal(err)
			}
			transport, err := driver.NewTransport(context.Background(), "00000000-0000-0000-0000-000000000001", SessionTokens{Req: token, Res: token}, true)
			if err != nil {
				t.Fatal(err)
			}
			defer transport.Close()
			if body, err := transport.ReadRaw(context.Background()); !errors.Is(err, ErrNoData) {
				if body != nil {
					body.Close()
				}
				t.Fatalf("empty session poll=%v", err)
			}
			var total int64
			for a, n := range m.RequestCounts() {
				total += n
				if a.Driver != network || a.Retry {
					t.Fatalf("unexpected labels %v", a)
				}
			}
			if total != 2 {
				t.Fatalf("SAS bootstrap+session attempts=%d want2", total)
			}
			if m.GetBytesReceived() != 0 {
				t.Fatal("empty SAS poll counted as payload")
			}
		})
	}
}

func TestSDKMetricsTransportFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL + "/testaccount")
	u.User = url.UserPassword("testaccount", "dGVzdC1rZXk=")
	m := NewDefaultMetrics()
	ctx := azruntime.WithRetryOptions(context.Background(), policy.RetryOptions{MaxRetries: -1})
	listener, err := Listen("azblob", u.String(), WithContext(ctx), WithMetrics(m))
	if err == nil || listener != nil {
		t.Fatalf("Listen=%v %v", listener, err)
	}
	counts := m.RequestCounts()
	if len(counts) != 1 {
		t.Fatalf("attempts=%v", counts)
	}
	for attempt, n := range counts {
		if n != 1 || !attempt.Failed || attempt.Retry || attempt.StatusCode != 0 {
			t.Fatalf("transport failure=%v count=%d", attempt, n)
		}
	}
	if m.GetWriteTransactionCount() != 1 {
		t.Fatalf("write attempts=%d", m.GetWriteTransactionCount())
	}
}
