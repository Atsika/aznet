package aznet

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestQueuePendingByteAccounting(t *testing.T) {
	cfg := applyConfig([]Option{WithBufferLimits(BufferLimits{Pending: 8})})
	defer cfg.cancel()
	tr := &queueTransport{cfg: cfg, pending: make(map[uint64][]byte)}
	if tr.ingestLocked(1, []byte("4567")) || tr.ingestLocked(1, []byte("duplicate")) {
		t.Fatal("duplicate counted")
	}
	if !tr.ingestLocked(2, []byte("overflow")) || tr.pendingBytes != 4 || len(tr.pending) != 1 {
		t.Fatal("overflow retained bytes")
	}
	if tr.ingestLocked(0, []byte("0123")) {
		t.Fatal("exact allowance rejected")
	}
	if got := string(tr.drainLocked()); got != "01234567" || tr.pendingBytes != 0 {
		t.Fatal(got, tr.pendingBytes)
	}
	if tr.ingestLocked(0, []byte("old")) || tr.pendingBytes != 0 {
		t.Fatal("consumed duplicate counted")
	}
}

// This is an investigation of the existing read-only bootstrap protocol, not a
// claim that Azure Queue supports pagination of PeekMessages. It demonstrates
// head-of-line blocking and its release when the owner deletes an earlier token.
func TestAzuriteQueueTokenCapacity(t *testing.T) {
	if os.Getenv("AZNET_AZURITE") != "1" {
		t.Skip("set AZNET_AZURITE=1")
	}
	const key = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
	u, _ := url.Parse("http://127.0.0.1:10001/devstoreaccount1")
	u.User = url.UserPassword("devstoreaccount1", key)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	cfg := applyConfig([]Option{WithEndpoints("h"+suffix, "t"+suffix)})
	defer cfg.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	d, err := (&queueFactory{}).NewDriver(NewEndpoint(u), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.CleanupBootstrap(ctx)
	ids := make([]string, 33)
	for i := range ids {
		ids[i] = uuid.NewString()
		if err := d.PostToken(ctx, ids[i], []byte("token")); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := d.GetToken(ctx, ids[32]); !errors.Is(err, ErrNoData) {
			t.Fatalf("33rd token peek: %v", err)
		}
	}
	if err := d.DeleteToken(ctx, ids[0]); err != nil {
		t.Fatal(err)
	}
	if got, err := d.GetToken(ctx, ids[32]); err != nil || string(got) != "token" {
		t.Fatalf("after deleting head: %q %v", got, err)
	}
	t.Log("33rd token hidden on three polls; visible after owner deletes one head token. Head-of-line blocking confirmed; permanent starvation not established.")
}

// Abandoned front tokens expire without the janitor or a client acknowledgement,
// allowing a later dialer to advance without expanding bootstrap permissions.
func TestAzuriteQueueAbandonedTokensExpire(t *testing.T) {
	if os.Getenv("AZNET_AZURITE") != "1" {
		t.Skip("set AZNET_AZURITE=1")
	}
	const key = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
	u, _ := url.Parse("http://127.0.0.1:10001/devstoreaccount1")
	u.User = url.UserPassword("devstoreaccount1", key)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	cfg := applyConfig([]Option{WithEndpoints("h"+suffix, "t"+suffix), WithConnectTimeout(time.Second)})
	defer cfg.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	driver, err := (&queueFactory{}).NewDriver(NewEndpoint(u), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer driver.CleanupBootstrap(ctx)
	for i := 0; i < 32; i++ {
		if err := driver.PostToken(ctx, uuid.NewString(), []byte("abandoned")); err != nil {
			t.Fatal(err)
		}
	}
	// Give the later token a longer setup window so it survives front expiry.
	cfg.connectTimeout = 5 * time.Second
	id := uuid.NewString()
	if err := driver.PostToken(ctx, id, []byte("later")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		got, err := driver.GetToken(ctx, id)
		if err == nil {
			if string(got) != "later" {
				t.Fatal(string(got))
			}
			break
		}
		if !errors.Is(err, ErrNoData) || time.Now().After(deadline) {
			t.Fatal("later dialer starved", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := driver.DeleteToken(ctx, id); err != nil {
		t.Fatal(err)
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
