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
	return dialViaProxy(ctx, &net.Dialer{}, pc, "storage.example:443")
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
	_, err := dialViaProxy(ctx, &net.Dialer{}, &ProxyConfig{URL: mustProxyURL(t, addr)}, "storage.example:443")
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
	d := &proxyDialer{resolve: func(_ context.Context, target *url.URL) (*ProxyConfig, error) {
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
	client := newProxyClient(func(context.Context, *url.URL) (*ProxyConfig, error) { return pc, nil })
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
