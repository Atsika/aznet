package aznet

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// startProxy runs handle for each connection to a local listener and returns
// its http:// address.
func startProxy(t *testing.T, handle func(conn net.Conn, br *bufio.Reader)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				handle(conn, bufio.NewReader(conn))
			}()
		}
	}()
	return "http://" + ln.Addr().String()
}

func readConnect(t *testing.T, br *bufio.Reader) *http.Request {
	t.Helper()
	req, err := http.ReadRequest(br)
	if err != nil {
		// The client closing its end is how these exchanges normally finish.
		return nil
	}
	if req.Method != http.MethodConnect {
		t.Errorf("method %s, want CONNECT", req.Method)
	}
	return req
}

func respond(conn net.Conn, status string, headers ...string) {
	fmt.Fprintf(conn, "HTTP/1.1 %s\r\n", status)
	for _, h := range headers {
		fmt.Fprintf(conn, "%s\r\n", h)
	}
	fmt.Fprint(conn, "Content-Length: 0\r\n\r\n")
}

func tunnelEcho(conn net.Conn, br *bufio.Reader) {
	respond(conn, "200 Connection Established")
	io.Copy(conn, br)
}

func connectThrough(t *testing.T, pc *ProxyConfig) (net.Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return dialViaProxy(ctx, &net.Dialer{}, pc, "storage.example:443", nil)
}

func mustProxyURL(t *testing.T, addr string) *url.URL {
	t.Helper()
	u, err := parseProxyURL(addr)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func assertEcho(t *testing.T, conn net.Conn) {
	t.Helper()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo %q, %v", buf, err)
	}
}

func TestProxyConnectWithoutAuth(t *testing.T) {
	var line string
	addr := startProxy(t, func(conn net.Conn, br *bufio.Reader) {
		req := readConnect(t, br)
		if req != nil {
			line = req.RequestURI
		}
		tunnelEcho(conn, br)
	})
	conn, err := connectThrough(t, &ProxyConfig{URL: mustProxyURL(t, addr)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	assertEcho(t, conn)
	if line != "storage.example:443" {
		t.Fatalf("CONNECT target %q", line)
	}
}

func TestProxyBasicIsPreemptive(t *testing.T) {
	var got string
	addr := startProxy(t, func(conn net.Conn, br *bufio.Reader) {
		if req := readConnect(t, br); req != nil {
			got = req.Header.Get("Proxy-Authorization")
		}
		tunnelEcho(conn, br)
	})
	conn, err := connectThrough(t, &ProxyConfig{URL: mustProxyURL(t, addr), Auth: ProxyBasic, User: "alice", Pass: "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cret")); got != want {
		t.Fatalf("Proxy-Authorization %q, want %q", got, want)
	}
}

func TestProxyAutoAnswersBasicChallengeOnSameConnection(t *testing.T) {
	var requests []string
	addr := startProxy(t, func(conn net.Conn, br *bufio.Reader) {
		for {
			req := readConnect(t, br)
			if req == nil {
				return
			}
			requests = append(requests, req.Header.Get("Proxy-Authorization"))
			if req.Header.Get("Proxy-Authorization") == "" {
				respond(conn, "407 Proxy Authentication Required", `Proxy-Authenticate: Basic realm="corp"`)
				continue
			}
			tunnelEcho(conn, br)
			return
		}
	})
	// Credentials come from the URL, as with HTTPS_PROXY=http://user:pass@host.
	u := mustProxyURL(t, strings.Replace(addr, "http://", "http://bob:pw@", 1))
	conn, err := connectThrough(t, &ProxyConfig{URL: u})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	assertEcho(t, conn)
	if len(requests) != 2 || requests[0] != "" || !strings.HasPrefix(requests[1], "Basic ") {
		t.Fatalf("requests %q", requests)
	}
}

func TestProxyRedialsWhenProxyClosesAfterChallenge(t *testing.T) {
	var mu sync.Mutex
	var conns int
	addr := startProxy(t, func(conn net.Conn, br *bufio.Reader) {
		mu.Lock()
		conns++
		mu.Unlock()
		req := readConnect(t, br)
		if req == nil {
			return
		}
		if req.Header.Get("Proxy-Authorization") == "" {
			respond(conn, "407 Proxy Authentication Required", `Proxy-Authenticate: Basic realm="corp"`, "Connection: close")
			return
		}
		tunnelEcho(conn, br)
	})
	conn, err := connectThrough(t, &ProxyConfig{URL: mustProxyURL(t, addr), User: "u", Pass: "p"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	assertEcho(t, conn)
	if conns != 2 {
		t.Fatalf("%d connections, want 2", conns)
	}
}

func TestProxyRejectedCredentialsDoNotLeak(t *testing.T) {
	addr := startProxy(t, func(conn net.Conn, br *bufio.Reader) {
		for readConnect(t, br) != nil {
			respond(conn, "407 Proxy Authentication Required", `Proxy-Authenticate: Basic realm="corp"`)
		}
	})
	_, err := connectThrough(t, &ProxyConfig{URL: mustProxyURL(t, addr), User: "alice", Pass: "hunter2"})
	if !errors.Is(err, ErrProxyAuthFailed) {
		t.Fatalf("err %v, want ErrProxyAuthFailed", err)
	}
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "alice") {
		t.Fatalf("credentials in error: %v", err)
	}
}

func TestProxyChallengeErrors(t *testing.T) {
	challenge := func(scheme string) string {
		return startProxy(t, func(conn net.Conn, br *bufio.Reader) {
			for readConnect(t, br) != nil {
				respond(conn, "407 Proxy Authentication Required", "Proxy-Authenticate: "+scheme)
			}
		})
	}
	if _, err := connectThrough(t, &ProxyConfig{URL: mustProxyURL(t, challenge(`Basic realm="x"`))}); !errors.Is(err, ErrProxyAuthRequired) {
		t.Fatalf("no credentials: %v", err)
	}
	if _, err := connectThrough(t, &ProxyConfig{URL: mustProxyURL(t, challenge(`Digest realm="x"`)), User: "u", Pass: "p"}); !errors.Is(err, ErrProxyAuthUnsupported) {
		t.Fatalf("digest only: %v", err)
	}
	// An explicit scheme the proxy does not offer is not silently swapped.
	if _, err := connectThrough(t, &ProxyConfig{URL: mustProxyURL(t, challenge(`Basic realm="x"`)), Auth: ProxyNTLM, User: "u", Pass: "p"}); !errors.Is(err, ErrProxyAuthUnsupported) {
		t.Fatalf("ntlm against basic-only: %v", err)
	}
}

func TestProxyRefusal(t *testing.T) {
	addr := startProxy(t, func(conn net.Conn, br *bufio.Reader) {
		readConnect(t, br)
		respond(conn, "403 Forbidden")
	})
	_, err := connectThrough(t, &ProxyConfig{URL: mustProxyURL(t, addr)})
	if !errors.Is(err, ErrProxyConnect) || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err %v", err)
	}
}

// ntlmChallenge is a minimal Type 2 message: no target name or target info.
func ntlmChallenge() []byte {
	var b bytes.Buffer
	b.WriteString("NTLMSSP\x00")
	binary.Write(&b, binary.LittleEndian, uint32(2))
	b.Write(make([]byte, 8))                                  // target name field
	binary.Write(&b, binary.LittleEndian, uint32(0x00000201)) // unicode, NTLM
	b.Write([]byte{1, 2, 3, 4, 5, 6, 7, 8})                   // server challenge
	b.Write(make([]byte, 8))                                  // reserved
	b.Write(make([]byte, 8))                                  // target info field
	return b.Bytes()
}

func TestProxyNTLMHandshakeStaysOnOneConnection(t *testing.T) {
	var conns int
	var type1, type3 []byte
	addr := startProxy(t, func(conn net.Conn, br *bufio.Reader) {
		conns++
		decode := func(req *http.Request) []byte {
			raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(req.Header.Get("Proxy-Authorization"), "NTLM "))
			if err != nil {
				t.Errorf("NTLM header: %v", err)
			}
			return raw
		}
		req := readConnect(t, br)
		respond(conn, "407 Proxy Authentication Required", "Proxy-Authenticate: NTLM")
		req = readConnect(t, br)
		type1 = decode(req)
		respond(conn, "407 Proxy Authentication Required", "Proxy-Authenticate: NTLM "+base64.StdEncoding.EncodeToString(ntlmChallenge()))
		req = readConnect(t, br)
		type3 = decode(req)
		tunnelEcho(conn, br)
	})
	conn, err := connectThrough(t, &ProxyConfig{URL: mustProxyURL(t, addr), User: `CORP\alice`, Pass: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	assertEcho(t, conn)
	if conns != 1 {
		t.Fatalf("%d connections, want 1", conns)
	}
	if !bytes.HasPrefix(type1, []byte("NTLMSSP\x00\x01\x00\x00\x00")) {
		t.Fatalf("type 1 message % x", type1)
	}
	if !bytes.HasPrefix(type3, []byte("NTLMSSP\x00\x03\x00\x00\x00")) {
		t.Fatalf("type 3 message % x", type3)
	}
	if !bytes.Contains(type3, utf16le("alice")) || !bytes.Contains(type3, utf16le("CORP")) {
		t.Fatalf("type 3 omits the user or domain: % x", type3)
	}
	if bytes.Contains(type3, []byte("pw")) {
		t.Fatal("type 3 carries the password")
	}
}

func utf16le(s string) []byte {
	out := make([]byte, 0, 2*len(s))
	for _, r := range s {
		out = append(out, byte(r), 0)
	}
	return out
}

func TestProxyAutoPrefersNTLMOverBasic(t *testing.T) {
	var second string
	addr := startProxy(t, func(conn net.Conn, br *bufio.Reader) {
		readConnect(t, br)
		respond(conn, "407 Proxy Authentication Required", `Proxy-Authenticate: Basic realm="x"`, "Proxy-Authenticate: NTLM")
		if req := readConnect(t, br); req != nil {
			second = req.Header.Get("Proxy-Authorization")
		}
		respond(conn, "403 Forbidden")
	})
	connectThrough(t, &ProxyConfig{URL: mustProxyURL(t, addr), User: "u", Pass: "p"})
	if !strings.HasPrefix(second, "NTLM ") {
		t.Fatalf("answered with %q, want NTLM", second)
	}
}

func TestProxyContextCancelStopsStalledProxy(t *testing.T) {
	addr := startProxy(t, func(conn net.Conn, br *bufio.Reader) {
		readConnect(t, br)
		time.Sleep(10 * time.Second) // never answers
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := dialViaProxy(ctx, &net.Dialer{}, &ProxyConfig{URL: mustProxyURL(t, addr)}, "storage.example:443", nil)
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}

func TestProxyFuncDirectAndTarget(t *testing.T) {
	direct, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	var seen []*url.URL
	d := &ProxyDialer{resolve: func(_ context.Context, target *url.URL) (*ProxyConfig, error) {
		seen = append(seen, target)
		return nil, nil // direct
	}}
	conn, err := d.DialContext(context.Background(), "tcp", direct.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	d.resolve(context.Background(), targetURL("acct.blob.core.windows.net:443"))
	if got := targetURL("acct.blob.core.windows.net:443"); got.Scheme != "https" || got.Host != "acct.blob.core.windows.net:443" {
		t.Fatalf("target %v", got)
	}
	if got := targetURL("127.0.0.1:10000"); got.Scheme != "http" {
		t.Fatalf("target %v", got)
	}
	if len(seen) != 2 || seen[0].Scheme != "http" {
		t.Fatalf("resolver saw %v", seen)
	}
}

// A real HTTPS request goes through the CONNECT tunnel, including TLS to the
// origin inside it, using the client the SDK would get.
func TestProxyClientReachesHTTPSOriginThroughTunnel(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "origin ok")
	}))
	defer origin.Close()
	originAddr := strings.TrimPrefix(origin.URL, "https://")

	var connectTo string
	addr := startProxy(t, func(conn net.Conn, br *bufio.Reader) {
		req := readConnect(t, br)
		if req == nil {
			return
		}
		connectTo = req.RequestURI
		up, err := net.Dial("tcp", originAddr)
		if err != nil {
			respond(conn, "502 Bad Gateway")
			return
		}
		defer up.Close()
		respond(conn, "200 Connection Established")
		go io.Copy(up, br)
		io.Copy(conn, up)
	})
	pc := &ProxyConfig{URL: mustProxyURL(t, addr)}
	client := newProxyClient(func(context.Context, *url.URL) (*ProxyConfig, error) { return pc, nil }, nil)
	// httptest's certificate is self-signed.
	tr := client.Transport.(*http.Transport)
	tr.TLSClientConfig.InsecureSkipVerify = true

	resp, err := client.Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "origin ok" || connectTo != originAddr {
		t.Fatalf("body %q via CONNECT %q", body, connectTo)
	}
}

func TestWithProxyAddress(t *testing.T) {
	for addr, want := range map[string]string{
		"proxy.corp:3128":          "http://proxy.corp:3128",
		"http://proxy.corp":        "http://proxy.corp:80",
		"https://proxy.corp":       "https://proxy.corp:443",
		"http://u:p@proxy.corp:81": "http://u:p@proxy.corp:81",
	} {
		u, err := parseProxyURL(addr)
		if err != nil || u.String() != want {
			t.Errorf("%q: %v, %v (want %s)", addr, u, err, want)
		}
	}
	for _, bad := range []string{"socks5://proxy.corp:1080", "http://", "", "ftp://x:1"} {
		cfg := applyConfig([]Option{WithProxy(bad, ProxyAuto, "", "")})
		if err := cfg.Validate(); !errors.Is(err, ErrInvalidProxy) {
			t.Errorf("%q accepted: %v", bad, err)
		}
		cfg.cancel()
	}
	cfg := applyConfig([]Option{WithProxy("proxy.corp:3128", ProxyBasic, "u", "p")})
	defer cfg.cancel()
	if err := cfg.Validate(); err != nil || cfg.httpClient == nil {
		t.Fatalf("valid proxy: %v, client %v", err, cfg.httpClient)
	}
	if applyConfig(nil).httpClient != nil {
		t.Fatal("default config must keep the SDK transport")
	}
}

func TestSDKClientOptionsUsesProxyClient(t *testing.T) {
	cfg := applyConfig([]Option{WithProxy("proxy.corp:3128", ProxyAuto, "", "")})
	defer cfg.cancel()
	ep := NewEndpoint(&url.URL{Scheme: "https", Host: "acct.blob.core.windows.net"})
	if sdkClientOptions(blobDriverName, cfg, ep).Transport == nil {
		t.Fatal("proxy transport not applied")
	}
	if sdkClientOptions(blobDriverName, applyConfig(nil), ep).Transport != nil {
		t.Fatal("default config overrides the transport")
	}
	if sdkClientOptions(blobDriverName, nil, ep).Transport != nil {
		t.Fatal("nil config overrides the transport")
	}
}

func TestProxyFuncResultIsValidated(t *testing.T) {
	var lastDial string
	srv := startProxy(t, func(conn net.Conn, br *bufio.Reader) {
		if req := readConnect(t, br); req != nil {
			lastDial = req.RequestURI
		}
		respond(conn, "403 Forbidden")
	})
	host := strings.TrimPrefix(srv, "http://")
	for _, bad := range []*url.URL{{Scheme: "socks5", Host: host}, {Scheme: "http"}, {Scheme: "ftp", Host: host}} {
		d := &ProxyDialer{resolve: func(context.Context, *url.URL) (*ProxyConfig, error) {
			return &ProxyConfig{URL: bad}, nil
		}}
		if _, err := d.DialContext(context.Background(), "tcp", "storage.example:443"); !errors.Is(err, ErrInvalidProxy) {
			t.Errorf("%v: %v", bad, err)
		}
	}
	// A resolver that returns a bare host, as Windows system settings do, means http.
	d := &ProxyDialer{resolve: func(context.Context, *url.URL) (*ProxyConfig, error) {
		return &ProxyConfig{URL: &url.URL{Host: host}}, nil
	}}
	if _, err := d.DialContext(context.Background(), "tcp", "storage.example:443"); !errors.Is(err, ErrProxyConnect) || lastDial != "storage.example:443" {
		t.Fatalf("bare host: %v, dialled %q", err, lastDial)
	}
}

func TestProxyLogTracesTheExchangeWithoutSecrets(t *testing.T) {
	addr := startProxy(t, func(conn net.Conn, br *bufio.Reader) {
		for {
			req := readConnect(t, br)
			if req == nil {
				return
			}
			if req.Header.Get("Proxy-Authorization") == "" {
				respond(conn, "407 Proxy Authentication Required", `Proxy-Authenticate: Basic realm="corp"`)
				continue
			}
			tunnelEcho(conn, br)
			return
		}
	})
	var mu sync.Mutex
	var lines []string
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	cfg := applyConfig([]Option{
		WithProxyLog(logf), // before the proxy: option order must not matter
		WithProxy(strings.Replace(addr, "http://", "http://alice:hunter2@", 1), ProxyAuto, "", ""),
	})
	defer cfg.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d := cfg.httpClient.Transport.(*http.Transport).DialContext
	conn, err := d(ctx, "tcp", "storage.example:443")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()

	log := strings.Join(lines, "\n")
	for _, want := range []string{
		"via http://127.0.0.1:", "auth=auto", "credentials supplied=true",
		"sending no credentials", "proxy replied 407", "proxy offers basic",
		"answering with basic", "sending Basic", "proxy replied 200", "tunnel established",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	for _, secret := range []string{"hunter2", "alice", base64.StdEncoding.EncodeToString([]byte("alice:hunter2"))} {
		if strings.Contains(log, secret) {
			t.Errorf("log leaks %q:\n%s", secret, log)
		}
	}
}

// The Azure SDK retries any dial error unless it carries NonRetriable. A
// rejected login must not be repeated (against a directory it counts toward
// locking the account), while an unreachable proxy may be.
func TestProxyAuthFailuresAreNotRetried(t *testing.T) {
	type nonRetriable interface{ NonRetriable() }
	rejecting := startProxy(t, func(conn net.Conn, br *bufio.Reader) {
		readConnect(t, br)
		respond(conn, "407 Proxy Authentication Required", "Proxy-Authenticate: Basic realm=\"x\"")
		readConnect(t, br)
		respond(conn, "407 Proxy Authentication Required", "Proxy-Authenticate: Basic realm=\"x\"")
	})
	refusing := startProxy(t, func(conn net.Conn, br *bufio.Reader) {
		readConnect(t, br)
		respond(conn, "403 Forbidden")
	})
	for _, tc := range []struct {
		name      string
		pc        *ProxyConfig
		want      error
		permanent bool
	}{
		{"wrong password", &ProxyConfig{URL: mustProxyURL(t, rejecting), User: "u", Pass: "bad"}, ErrProxyAuthFailed, true},
		{"no credentials", &ProxyConfig{URL: mustProxyURL(t, rejecting)}, ErrProxyAuthRequired, true},
		{"unsupported scheme", &ProxyConfig{URL: mustProxyURL(t, rejecting), Auth: ProxyNTLM, User: "u", Pass: "p"}, ErrProxyAuthUnsupported, true},
		{"refused", &ProxyConfig{URL: mustProxyURL(t, refusing)}, ErrProxyConnect, false},
		{"unreachable", &ProxyConfig{URL: mustProxyURL(t, "127.0.0.1:1")}, ErrProxyConnect, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &ProxyDialer{
				resolve: func(context.Context, *url.URL) (*ProxyConfig, error) { return tc.pc, nil },
				dialer:  net.Dialer{Timeout: 2 * time.Second},
			}
			_, err := d.DialContext(context.Background(), "tcp", "storage.example:443")
			if !errors.Is(err, tc.want) {
				t.Fatalf("err %v, want %v", err, tc.want)
			}
			var nr nonRetriable
			if got := errors.As(err, &nr); got != tc.permanent {
				t.Fatalf("non-retriable %t, want %t (%v)", got, tc.permanent, err)
			}
		})
	}
}

// scriptedNegotiate sends one token and then reports the proxy's bare 407 as a
// rejection, as SSPI does.
type scriptedNegotiate struct{ sent bool }

func (*scriptedNegotiate) scheme() string        { return "negotiate" }
func (*scriptedNegotiate) connectionBound() bool { return true }
func (a *scriptedNegotiate) respond(string) (string, error) {
	if a.sent {
		return "", ErrProxyAuthFailed
	}
	a.sent = true
	return "Negotiate dG9rZW4=", nil
}

// A proxy that has rejected a Negotiate token keeps that state for the
// connection and answers an NTLM message there with a fresh challenge list, so
// the fallback to NTLM must happen on a new connection (seen with Squid).
func TestProxyAutoFallsBackToNTLMOnANewConnectionAfterNegotiateIsRejected(t *testing.T) {
	old := newNegotiateAuthFunc
	newNegotiateAuthFunc = func(*ProxyConfig, string, string) (proxyAuthenticator, error) { return &scriptedNegotiate{}, nil }
	t.Cleanup(func() { newNegotiateAuthFunc = old })

	var conns atomic.Int32
	var ntlmOnFirst atomic.Bool
	addr := startProxy(t, func(conn net.Conn, br *bufio.Reader) {
		n := conns.Add(1)
		offer := []string{"Proxy-Authenticate: Negotiate", "Proxy-Authenticate: NTLM"}
		readConnect(t, br)
		respond(conn, "407 Proxy Authentication Required", offer...)
		if n == 1 {
			req := readConnect(t, br) // the Negotiate token
			if req == nil || !strings.HasPrefix(req.Header.Get("Proxy-Authorization"), "Negotiate ") {
				t.Errorf("first connection: %v", req)
				return
			}
			respond(conn, "407 Proxy Authentication Required", offer...)
			// Anything further on this connection gets the generic list.
			if req := readConnect(t, br); req != nil {
				ntlmOnFirst.Store(strings.HasPrefix(req.Header.Get("Proxy-Authorization"), "NTLM "))
				respond(conn, "407 Proxy Authentication Required", offer...)
			}
			return
		}
		if req := readConnect(t, br); req == nil || !strings.HasPrefix(req.Header.Get("Proxy-Authorization"), "NTLM ") {
			t.Errorf("second connection: %v", req)
			return
		}
		respond(conn, "407 Proxy Authentication Required", "Proxy-Authenticate: NTLM "+base64.StdEncoding.EncodeToString(ntlmChallenge()))
		if req := readConnect(t, br); req == nil || !strings.HasPrefix(req.Header.Get("Proxy-Authorization"), "NTLM ") {
			t.Errorf("second connection, authenticate message: %v", req)
			return
		}
		tunnelEcho(conn, br)
	})
	conn, err := connectThrough(t, &ProxyConfig{URL: mustProxyURL(t, addr), User: "alice", Pass: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	assertEcho(t, conn)
	if ntlmOnFirst.Load() {
		t.Error("NTLM was attempted on the connection that rejected Negotiate")
	}
	if got := conns.Load(); got != 2 {
		t.Errorf("%d connections, want 2", got)
	}
}

// A tunnel whose first bytes arrive with the CONNECT response is wrapped to
// replay them; it must still half-close so relays can forward an EOF.
func TestProxyDialerTunnelHalfCloses(t *testing.T) {
	addr := startProxy(t, func(conn net.Conn, br *bufio.Reader) {
		if readConnect(t, br) == nil {
			return
		}
		fmt.Fprint(conn, "HTTP/1.1 200 Connection Established\r\n\r\nhi")
		io.Copy(io.Discard, br) // until the client half-closes
		conn.Write([]byte("bye"))
	})
	pc := &ProxyConfig{URL: mustProxyURL(t, addr)}
	d := NewProxyDialer(func(context.Context, *url.URL) (*ProxyConfig, error) { return pc, nil }, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", "app.internal:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "hi" {
		t.Fatalf("early bytes %q, %v", buf, err)
	}
	cw, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatalf("%T cannot half-close", conn)
	}
	if err := cw.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(conn)
	if err != nil || string(rest) != "bye" {
		t.Fatalf("after half-close %q, %v", rest, err)
	}
}
