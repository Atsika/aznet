package aznet

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

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

func TestInvalidCredentialDurations(t *testing.T) {
	for _, duration := range []time.Duration{-time.Hour, 0, time.Nanosecond, 999 * time.Millisecond} {
		cfg := applyConfig([]Option{WithSessionDuration(duration)})
		if err := cfg.Validate(); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("duration%v Validate=%v", duration, err)
		}
		cfg.cancel()
		l := &Listener{}
		if _, err := l.ConnectionStringFor(duration); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("bootstrap duration%v error=%v", duration, err)
		}
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

func TestUnknownSessionExpirySerialization(t *testing.T) {
	raw, err := json.Marshal(SessionTokens{Req: "custom", Res: "opaque"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "expires_at") {
		t.Fatal("unknown metadata must be absent from handshake")
	}
	var legacy SessionTokens
	if err := json.Unmarshal([]byte(`{"req":"custom","res":"opaque"}`), &legacy); err != nil || !legacy.ExpiresAt.IsZero() {
		t.Fatalf("legacy tokens: %v %v", legacy, err)
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
