package aznet

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
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

func (b *cancelBody) Close() error { return nil }

type failingCloseTransport struct {
	reviewTransport
	calls   int
	failure error
}

func (t *failingCloseTransport) Close() error {
	t.calls++
	return t.failure
}

func TestRollbackDoesNotRetryTransportClose(t *testing.T) {
	failure := &azcore.ResponseError{StatusCode: 503}
	transport := &failingCloseTransport{failure: failure}
	driver := &sessionDriver{transport: func(context.Context) (Transport, error) { return transport, errors.New("partial acquisition") }}
	listener := sessionListener(t, driver, 1)
	if _, err := listener.Accept(); !errors.Is(err, failure) {
		t.Fatalf("lost close failure: %v", err)
	}
	if transport.calls != 1 {
		t.Fatalf("Close called %d times for one acquisition", transport.calls)
	}
}

func TestAzuriteBatchedSessionLifecycle(t *testing.T) {
	if os.Getenv("AZNET_AZURITE") != "1" {
		t.Skip("set AZNET_AZURITE=1")
	}
	const key = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
	for i, network := range []string{"azblob", "azqueue", "aztable"} {
		t.Run(network, func(t *testing.T) {
			u, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d/devstoreaccount1", 10000+i))
			u.User = url.UserPassword("devstoreaccount1", key)
			suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			opts := []Option{WithContext(ctx), WithEndpoints("h"+suffix, "t"+suffix), WithSessionDuration(2 * time.Hour), WithPing(0), WithDataPoll(5 * time.Millisecond), WithAcceptPoll(5 * time.Millisecond)}
			listener, err := Listen(network, u.String(), opts...)
			if err != nil {
				t.Fatal(err)
			}
			l := listener.(*Listener)
			defer func() {
				l.Close()
				cleanupCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
				defer done()
				if err := l.CleanupBootstrap(cleanupCtx); err != nil {
					t.Error(err)
				}
			}()
			issuedBefore := time.Now().UTC().Truncate(time.Second)
			address, err := l.ConnectionStringFor(time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			const count = 8
			clients := make(chan net.Conn, count)
			errs := make(chan error, count)
			for j := 0; j < count; j++ {
				go func() {
					c, err := Dial(network, address, opts...)
					if err != nil {
						errs <- err
						return
					}
					clients <- c
				}()
			}
			// Let requests accumulate so GetHandshakes returns a real batch.
			time.Sleep(100 * time.Millisecond)
			var accepted []*Conn
			for j := 0; j < count; j++ {
				c, err := l.Accept()
				if err != nil {
					t.Fatal(err)
				}
				accepted = append(accepted, c.(*Conn))
			}
			var dialed []net.Conn
			for j := 0; j < count; j++ {
				select {
				case c := <-clients:
					dialed = append(dialed, c)
					defer c.Close()
					if _, err := c.Write([]byte("ready")); err != nil {
						t.Fatal(err)
					}
				case err := <-errs:
					t.Fatal(err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			for _, client := range dialed {
				expiry, known := GetSessionExpiry(client)
				if !known || expiry.Before(issuedBefore.Add(2*time.Hour)) || expiry.After(time.Now().UTC().Add(2*time.Hour)) {
					t.Fatalf("dial expiry=%v known=%v", expiry, known)
				}
				matched := false
				for _, server := range accepted {
					if server.id == client.(*Conn).id {
						other, ok := GetSessionExpiry(server)
						if !ok || !other.Equal(expiry) {
							t.Fatal("dial/accept metadata differs")
						}
						matched = true
					}
				}
				if !matched {
					t.Fatal("session metadata peer missing")
				}
			}
			for _, c := range accepted {
				buf := make([]byte, 5)
				if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ready" {
					t.Fatal(string(buf), err)
				}
			}
			// Full accepted Close destroys storage. Half-close first, then wait
			// for application completion to establish final response delivery.
			for _, c := range accepted {
				if _, err := c.Write([]byte("reply")); err != nil {
					t.Fatal(err)
				}
				if err := c.CloseWrite(); err != nil {
					t.Fatal(err)
				}
			}
			for _, c := range dialed {
				reply, err := io.ReadAll(c)
				if err != nil || string(reply) != "reply" {
					t.Fatalf("half-close reply: %q %v", reply, err)
				}
				if _, err := c.Write([]byte("done")); err != nil {
					t.Fatal(err)
				}
			}
			for _, c := range accepted {
				ack := make([]byte, 4)
				if _, err := io.ReadFull(c, ack); err != nil || string(ack) != "done" {
					t.Fatalf("application completion: %q %v", ack, err)
				}
			}
			// No janitor tick has elapsed. Close must reclaim all accepted sessions.
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			for _, c := range accepted {
				var checks []error
				switch d := l.driver.(*metricsDriver).Driver.(type) {
				case *blobDriver:
					_, err := d.client.NewContainerClient(c.id).GetProperties(ctx, nil)
					checks = append(checks, err)
				case *queueDriver:
					for _, prefix := range []string{d.cfg.reqPrefix, d.cfg.resPrefix} {
						_, err := d.client.NewQueueClient(prefix+"-"+c.id).GetProperties(ctx, nil)
						checks = append(checks, err)
					}
				case *tableDriver:
					for _, prefix := range []string{d.cfg.reqPrefix, d.cfg.resPrefix} {
						_, err := d.client.NewClient(prefix+strings.ReplaceAll(c.id, "-", "")).GetAccessPolicy(ctx, nil)
						checks = append(checks, err)
					}
				}
				for _, err := range checks {
					var response *azcore.ResponseError
					if !errors.As(err, &response) || response.StatusCode != 404 {
						t.Fatalf("session resource remains: %v", err)
					}
				}
			}
			// Shared bootstrap remains usable by a later listener generation.
			if _, _, err := l.driver.CreateBootstrapTokens(); err != nil {
				t.Fatal(err)
			}
			if _, err := l.driver.GetHandshakes(ctx); err != nil {
				t.Fatal("bootstrap was removed", err)
			}
		})
	}
}

func tokenExpiry(t *testing.T, token string) time.Time {
	t.Helper()
	values, err := url.ParseQuery(token)
	if err != nil {
		t.Fatal(err)
	}
	expiry, err := time.Parse(time.RFC3339, values.Get("se"))
	if err != nil {
		t.Fatal(err)
	}
	return expiry
}

func TestCredentialDurationsAndExactSessionExpiry(t *testing.T) {
	for _, network := range []string{"azblob", "azqueue", "aztable"} {
		t.Run(network, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(201)
				if network == "aztable" {
					fmt.Fprint(w, `{"TableName":"fixture"}`)
				}
			}))
			defer server.Close()
			u, _ := url.Parse(server.URL + "/testaccount")
			u.User = url.UserPassword("testaccount", "dGVzdC1rZXk=")
			var clock atomic.Int64
			initial := time.Date(2026, 10, 5, 15, 0, 0, 987654321, time.UTC)
			clock.Store(initial.UnixNano())
			options := []Option{WithSessionDuration(2 * time.Hour), func(c *Config) { c.now = func() time.Time { return time.Unix(0, clock.Load()) } }}
			driver, ep, cfg, err := initialize(network, u.String(), options)
			if err != nil {
				t.Fatal(err)
			}
			defer cfg.cancel()
			l := &Listener{driver: driver, ep: ep, cfg: cfg}
			for _, duration := range []time.Duration{time.Hour, 7 * 24 * time.Hour} {
				address, err := l.ConnectionStringFor(duration)
				if err != nil {
					t.Fatal(err)
				}
				parsed, _ := url.Parse(address)
				for _, name := range []string{cfg.handshakeEndpoint, cfg.tokenEndpoint} {
					raw, err := base64.URLEncoding.DecodeString(parsed.Query().Get(name))
					if err != nil {
						t.Fatal(err)
					}
					if got, want := tokenExpiry(t, string(raw)), initial.Add(duration).Truncate(time.Second); !got.Equal(want) {
						t.Fatalf("bootstrap expiry=%v want%v", got, want)
					}
				}
			}
			for i := 0; i < 2; i++ {
				now := initial.Add(time.Duration(i) * time.Minute)
				clock.Store(now.UnixNano())
				tokens, err := driver.CreateSession(context.Background(), fmt.Sprintf("00000000-0000-0000-0000-%012d", i))
				if err != nil {
					t.Fatal(err)
				}
				req, res := tokenExpiry(t, tokens.Req), tokenExpiry(t, tokens.Res)
				want := req
				if res.Before(want) {
					want = res
				}
				if !tokens.ExpiresAt.Equal(want) || !want.Equal(now.Add(2*time.Hour).Truncate(time.Second)) {
					t.Fatalf("session expiry=%v signed=%v want%v", tokens.ExpiresAt, want, now.Add(2*time.Hour).Truncate(time.Second))
				}
			}
			// Independent bootstrap issuance must not mutate policy during session creation.
			var wg sync.WaitGroup
			for i := range 20 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, err := l.ConnectionStringFor(time.Duration(i+1) * time.Hour); err != nil {
						t.Error(err)
					}
				}()
			}
			for i := range 20 {
				tokens, err := driver.CreateSession(context.Background(), fmt.Sprintf("00000000-0000-0000-0001-%012d", i))
				if err != nil {
					t.Fatal(err)
				}
				if !tokens.ExpiresAt.Equal(initial.Add(time.Minute + 2*time.Hour).Truncate(time.Second)) {
					t.Error("bootstrap issuance changed session policy")
				}
			}
			wg.Wait()
			// Required tokens may be signed on opposite sides of a second boundary.
			var calls atomic.Int64
			cfg.now = func() time.Time { return initial.Add(time.Duration(calls.Add(1)) * time.Second) }
			tokens, err := driver.CreateSession(context.Background(), "00000000-0000-0000-0002-000000000001")
			if err != nil {
				t.Fatal(err)
			}
			req, res := tokenExpiry(t, tokens.Req), tokenExpiry(t, tokens.Res)
			want := initial.Add(time.Second + 2*time.Hour).Truncate(time.Second)
			if !req.Equal(want) || !tokens.ExpiresAt.Equal(want) {
				t.Fatalf("earliest signed expiry: req=%v res=%v metadata=%v want=%v", req, res, tokens.ExpiresAt, want)
			}
			if network != "azblob" && !res.Equal(want.Add(time.Second)) {
				t.Fatal("fixture did not cross the signing boundary")
			}

		})
	}
}

func TestAcceptSessionExpiryCapability(t *testing.T) {
	for _, expiry := range []time.Time{{}, time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)} {
		t.Run(expiry.String(), func(t *testing.T) {
			driver := &sessionDriver{create: func(context.Context, string) (SessionTokens, error) { return SessionTokens{ExpiresAt: expiry}, nil }}
			listener := sessionListener(t, driver, 1)
			conn, err := listener.Accept()
			if err != nil {
				t.Fatal(err)
			}
			got, known := GetSessionExpiry(conn)
			if known != !expiry.IsZero() || !got.Equal(expiry) {
				t.Fatalf("expiry=%v known=%v", got, known)
			}
			// Even past metadata must not add an implicit disconnect/renewal timer.
			if n, err := conn.Write([]byte("still permitted")); err != nil || n != 15 {
				t.Fatalf("informational expiry changed I/O: %d %v", n, err)
			}
		})
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if _, known := GetSessionExpiry(a); known {
		t.Fatal("ordinary connection expiry must be unknown")
	}
}

type legacyBootstrapDriver struct{ sessionDriver }

func (*legacyBootstrapDriver) CreateBootstrapTokens() (string, string, error) {
	return "legacy-h", "legacy-t", nil
}

func TestLegacyDriverUnknownBootstrapDuration(t *testing.T) {
	cfg := defaultConfig()
	defer cfg.cancel()
	u, _ := url.Parse("https://test.blob.core.windows.net")
	listener := &Listener{driver: &metricsDriver{Driver: &legacyBootstrapDriver{}, m: cfg.metrics}, ep: NewEndpoint(u), cfg: cfg}
	if _, err := listener.ConnectionString(); err != nil {
		t.Fatal(err)
	}
	if _, err := listener.ConnectionStringFor(time.Hour); !errors.Is(err, ErrBootstrapDurationUnsupported) {
		t.Fatalf("unsupported duration: %v", err)
	}
}

func TestAuthorizationErrorsRemainSpecific(t *testing.T) {
	// A timestamp alone cannot classify every403 as expiration. The original SDK
	// code remains inspectable for both time-validity and permission failures.
	for _, code := range []string{"AuthenticationFailed", "AuthorizationPermissionMismatch"} {
		t.Run(code, func(t *testing.T) {
			failure := &azcore.ResponseError{StatusCode: 403, ErrorCode: code}
			driver := &sessionDriver{create: func(context.Context, string) (SessionTokens, error) { return SessionTokens{}, failure }}
			listener := sessionListener(t, driver, 1)
			_, err := listener.Accept()
			var got *azcore.ResponseError
			var accept *AcceptError
			if !errors.As(err, &got) || got.ErrorCode != code || !errors.Is(err, failure) || !errors.As(err, &accept) || accept.Temporary() {
				t.Fatalf("authorization error lost or misclassified: %v", err)
			}
		})
	}
}
