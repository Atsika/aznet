package aznet

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Azure/go-ntlmssp"
	"golang.org/x/net/http2"
)

// ProxyAuth selects how aznet authenticates to an egress proxy.
type ProxyAuth int

const (
	// ProxyAuto picks the strongest scheme the proxy offers that the available
	// credentials allow: Negotiate, then NTLM, then Basic. Nothing is sent
	// until the proxy has said which schemes it accepts.
	ProxyAuto ProxyAuth = iota
	// ProxyBasic sends the credentials with the first request. Use it only
	// when the proxy is known to ask for Basic.
	ProxyBasic
	// ProxyNTLM authenticates with NTLM and explicit credentials.
	ProxyNTLM
	// ProxyNegotiate authenticates with SPNEGO (Kerberos, falling back to NTLM
	// where the platform provides it).
	ProxyNegotiate
)

func (a ProxyAuth) String() string {
	switch a {
	case ProxyBasic:
		return "basic"
	case ProxyNTLM:
		return "ntlm"
	case ProxyNegotiate:
		return "negotiate"
	default:
		return "auto"
	}
}

// ProxyConfig describes the egress proxy to use for one connection.
type ProxyConfig struct {
	// URL is the proxy address. Its scheme is http or https (TLS to the proxy
	// itself); any credentials in its user information are used when User and
	// Pass are empty.
	URL *url.URL
	// Auth selects the authentication scheme.
	Auth ProxyAuth
	// User and Pass are the proxy credentials. NTLM accepts DOMAIN\user and
	// user@domain. Both empty means no explicit credentials: Negotiate then
	// uses the identity of the running process where the platform allows it.
	User, Pass string
	// SPN is the Kerberos service name, HTTP/ plus the proxy's host name when
	// empty. Set it when the proxy is reached by an alias or an address.
	SPN string
}

// ProxyFunc chooses the egress proxy for a new connection to target, a URL
// whose scheme is https for port 443 and http otherwise. A nil config, with a
// nil error, connects directly. It is called for every new connection, so a
// reconnect can pick a different proxy.
type ProxyFunc func(ctx context.Context, target *url.URL) (*ProxyConfig, error)

// WithProxy sends every storage request through the HTTP egress proxy at
// address, using HTTP CONNECT. address is a URL such as
// "http://proxy.corp:8080"; a bare host:port means http. user and pass may be
// empty, in which case credentials in the address are used, and failing that
// the proxy is tried without any (see ProxyConfig.User).
//
// Only HTTP and HTTPS proxies are supported. Plain-HTTP storage endpoints
// (such as an emulator) are reached through CONNECT too, which most proxies
// restrict to port 443.
func WithProxy(address string, auth ProxyAuth, user, pass string) Option {
	u, err := parseProxyURL(address)
	return func(c *Config) {
		if err != nil {
			c.proxyErr = err
			return
		}
		pc := &ProxyConfig{URL: u, Auth: auth, User: user, Pass: pass}
		c.setProxy(func(context.Context, *url.URL) (*ProxyConfig, error) { return pc, nil })
	}
}

// WithProxyFunc is WithProxy with the proxy chosen per connection.
func WithProxyFunc(fn ProxyFunc) Option {
	return func(c *Config) {
		if fn != nil {
			c.setProxy(fn)
		}
	}
}

// WithProxyLog reports how connections are made through the proxy: which proxy
// was chosen, the CONNECT exchange, the schemes the proxy offers and which one
// answered. Credentials, tokens and user names are never included. It has no
// effect without WithProxy or WithProxyFunc, and may be given in any order.
func WithProxyLog(logf func(format string, args ...any)) Option {
	return func(c *Config) { c.proxyLog = logf }
}

func (c *Config) setProxy(fn ProxyFunc) {
	c.proxyFn = fn
}

// finishProxy builds the HTTP client once every option has been applied.
func (c *Config) finishProxy() {
	if c.proxyFn != nil {
		c.httpClient = newProxyClient(c.proxyFn, c.proxyLog)
	}
}

// proxyLabel names a proxy for logs: scheme and address, never credentials.
func proxyLabel(u *url.URL) string {
	return u.Scheme + "://" + u.Host
}

// parseProxyURL accepts "host:port" as shorthand for an http proxy.
func parseProxyURL(address string) (*url.URL, error) {
	if !strings.Contains(address, "://") {
		address = "http://" + address
	}
	u, err := url.Parse(address)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid proxy address", ErrInvalidProxy)
	}
	return normalizeProxyURL(u)
}

// normalizeProxyURL returns a copy of u that is safe to dial: http or https
// (a missing scheme means http), with the scheme's default port filled in.
func normalizeProxyURL(in *url.URL) (*url.URL, error) {
	u := *in
	if u.Scheme == "" {
		u.Scheme = "http"
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("%w: invalid proxy address", ErrInvalidProxy)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return nil, fmt.Errorf("%w: unsupported scheme %q (http and https only)", ErrInvalidProxy, u.Scheme)
	}
	if u.Port() == "" {
		port := "80"
		if u.Scheme == "https" {
			port = "443"
		}
		u.Host = net.JoinHostPort(u.Hostname(), port)
	}
	return &u, nil
}

// newProxyClient builds the HTTP client the SDK clients use when a proxy is
// configured. It mirrors the SDK's default transport so behaviour only differs
// in how the TCP connection is established.
func newProxyClient(fn ProxyFunc, logf func(string, ...any)) *http.Client {
	d := &proxyDialer{resolve: fn, logf: logf, dialer: net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}}
	tr := &http.Transport{
		DialContext:           d.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, Renegotiation: tls.RenegotiateFreelyAsClient},
	}
	if h2, err := http2.ConfigureTransports(tr); err == nil {
		h2.ReadIdleTimeout = 10 * time.Second
		h2.PingTimeout = 5 * time.Second
	}
	return &http.Client{Transport: tr}
}

type proxyDialer struct {
	resolve ProxyFunc
	logf    func(string, ...any)
	dialer  net.Dialer
}

func (d *proxyDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	pc, err := d.resolve(ctx, targetURL(addr))
	if err != nil {
		return nil, err
	}
	if pc == nil || pc.URL == nil {
		if d.logf != nil {
			d.logf("proxy: %s: direct connection", addr)
		}
		return d.dialer.DialContext(ctx, network, addr)
	}
	// A resolver can return anything, such as a socks5 URL from the
	// environment, so check it the way WithProxy does.
	u, err := normalizeProxyURL(pc.URL)
	if err != nil {
		return nil, err
	}
	resolved := *pc
	resolved.URL = u
	return dialViaProxy(ctx, &d.dialer, &resolved, addr, d.logf)
}

// targetURL describes the destination for a ProxyFunc.
func targetURL(addr string) *url.URL {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return &url.URL{Scheme: "http", Host: addr}
	}
	scheme := "http"
	if port == "443" {
		scheme = "https"
	}
	return &url.URL{Scheme: scheme, Host: net.JoinHostPort(host, port)}
}

// proxyAuthenticator answers a proxy's 407 challenges for one scheme.
type proxyAuthenticator interface {
	// scheme is the lower-case name used in Proxy-Authenticate.
	scheme() string
	// connectionBound reports that the credentials authenticate the TCP
	// connection itself, so the exchange cannot move to a new one.
	connectionBound() bool
	// respond returns the Proxy-Authorization value for the scheme's challenge
	// payload (empty for the first one), or ErrProxyAuthFailed once the scheme
	// has no more answers.
	respond(challenge string) (string, error)
}

// proxyCredentials returns the explicit credentials, falling back to those in
// the proxy URL.
func proxyCredentials(pc *ProxyConfig) (user, pass string) {
	user, pass = pc.User, pc.Pass
	if user == "" && pass == "" && pc.URL != nil && pc.URL.User != nil {
		user = pc.URL.User.Username()
		pass, _ = pc.URL.User.Password()
	}
	return user, pass
}

// parseChallenges maps each offered scheme (lower case) to its payload.
// Challenges are expected one per header value, as proxies send them.
func parseChallenges(values []string) map[string]string {
	out := make(map[string]string, len(values))
	for _, v := range values {
		scheme, payload, _ := strings.Cut(strings.TrimSpace(v), " ")
		if scheme == "" {
			continue
		}
		scheme = strings.ToLower(scheme)
		if _, dup := out[scheme]; !dup {
			out[scheme] = strings.TrimSpace(payload)
		}
	}
	return out
}

func authSchemeName(a ProxyAuth) string {
	switch a {
	case ProxyNegotiate:
		return "negotiate"
	case ProxyNTLM:
		return "ntlm"
	default:
		return "basic"
	}
}

// selectProxyAuth chooses the scheme to answer the first challenge with.
func selectProxyAuth(pc *ProxyConfig, user, pass string, offered map[string]string) (proxyAuthenticator, error) {
	order := []ProxyAuth{pc.Auth}
	if pc.Auth == ProxyAuto {
		order = []ProxyAuth{ProxyNegotiate, ProxyNTLM, ProxyBasic}
	}
	var needCreds bool
	var lastErr error
	for _, a := range order {
		if _, ok := offered[authSchemeName(a)]; !ok {
			continue
		}
		switch a {
		case ProxyNegotiate:
			auth, err := newNegotiateAuth(pc, user, pass)
			if err != nil {
				lastErr = err
				continue
			}
			return auth, nil
		case ProxyNTLM:
			if user == "" {
				// Without credentials only the platform can answer, as the
				// logged-on user.
				if auth, err := newAmbientNTLMAuth(); err == nil {
					return auth, nil
				}
				needCreds = true
				continue
			}
			return &ntlmAuth{user: user, pass: pass}, nil
		case ProxyBasic:
			if user == "" {
				needCreds = true
				continue
			}
			return &basicAuth{user: user, pass: pass}, nil
		}
	}
	switch {
	case needCreds:
		return nil, ErrProxyAuthRequired
	case lastErr != nil:
		return nil, lastErr
	}
	return nil, fmt.Errorf("%w: proxy offers %s", ErrProxyAuthUnsupported, offeredSchemes(offered))
}

// offeredSchemes lists the schemes in a challenge set, for messages.
func offeredSchemes(offered map[string]string) string {
	names := make([]string, 0, len(offered))
	for name := range offered {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

type basicAuth struct {
	user, pass string
	sent       bool
}

func (*basicAuth) scheme() string        { return "basic" }
func (*basicAuth) connectionBound() bool { return false }

func (a *basicAuth) respond(string) (string, error) {
	if a.sent {
		return "", ErrProxyAuthFailed // the proxy rejected the credentials
	}
	a.sent = true
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(a.user+":"+a.pass)), nil
}

// ntlmAuth runs the three-message NTLM exchange with explicit credentials.
type ntlmAuth struct {
	user, pass string
	step       int
}

func (*ntlmAuth) scheme() string        { return "ntlm" }
func (*ntlmAuth) connectionBound() bool { return true }

func (a *ntlmAuth) respond(challenge string) (string, error) {
	switch a.step {
	case 0:
		a.step = 1
		msg, err := ntlmssp.NewNegotiateMessage("", "")
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrProxyAuthFailed, err)
		}
		return "NTLM " + base64.StdEncoding.EncodeToString(msg), nil
	case 1:
		a.step = 2
		raw, err := base64.StdEncoding.DecodeString(challenge)
		if err != nil || len(raw) == 0 {
			return "", fmt.Errorf("%w: malformed NTLM challenge", ErrProxyAuthFailed)
		}
		msg, err := ntlmssp.NewAuthenticateMessage(raw, a.user, a.pass, nil)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrProxyAuthFailed, err)
		}
		return "NTLM " + base64.StdEncoding.EncodeToString(msg), nil
	}
	return "", ErrProxyAuthFailed // the proxy rejected the credentials
}

const (
	// maxProxyRounds bounds the 407 exchanges on one connection: NTLM needs
	// three requests, so six leaves room for one retry.
	maxProxyRounds = 6
	// proxyHandshakeTimeout bounds the CONNECT exchange when the caller's
	// context has no deadline.
	proxyHandshakeTimeout = 30 * time.Second
)

// errProxyReconnect asks dialViaProxy to retry on a fresh connection because
// the proxy closed this one after a challenge.
var errProxyReconnect = errors.New("proxy closed the connection")

// proxyState carries authentication progress across reconnects.
type proxyState struct {
	auth  proxyAuthenticator
	authz string
	logf  func(string, ...any)
}

func (st *proxyState) log(format string, args ...any) {
	if st.logf != nil {
		st.logf("proxy: "+format, args...)
	}
}

// release frees platform resources held by the authenticator once the tunnel
// is up or has failed.
func (st *proxyState) release() {
	if r, ok := st.auth.(interface{ release() }); ok {
		r.release()
	}
}

// proxySPN is the Kerberos service name for the proxy.
func proxySPN(pc *ProxyConfig) string {
	if pc.SPN != "" {
		return pc.SPN
	}
	return "HTTP/" + strings.ToLower(pc.URL.Hostname())
}

// dialViaProxy opens a tunnel to addr through the proxy with HTTP CONNECT,
// answering any authentication challenge on the same connection. The caller's
// TLS then runs inside the tunnel.
func dialViaProxy(ctx context.Context, dialer *net.Dialer, pc *ProxyConfig, addr string, logf func(string, ...any)) (net.Conn, error) {
	user, pass := proxyCredentials(pc)
	st := &proxyState{logf: logf}
	defer st.release()
	st.log("%s: via %s (auth=%s, credentials supplied=%t)", addr, proxyLabel(pc.URL), pc.Auth, user != "")
	if pc.Auth == ProxyBasic && user != "" {
		st.auth = &basicAuth{user: user, pass: pass}
		st.authz, _ = st.auth.respond("")
	}
	for attempt := 0; ; attempt++ {
		conn, err := dialProxyConn(ctx, dialer, pc)
		if err != nil {
			st.log("%s: cannot reach the proxy: %v", addr, err)
			return nil, err
		}
		tunnel, err := connectOnce(ctx, conn, pc, user, pass, addr, st)
		if err == nil {
			st.log("%s: tunnel established", addr)
			return tunnel, nil
		}
		conn.Close()
		if errors.Is(err, errProxyReconnect) {
			if attempt < 2 {
				st.log("%s: proxy closed the connection after the challenge, reconnecting", addr)
				continue
			}
			err = fmt.Errorf("%w: proxy keeps closing the connection", ErrProxyConnect)
		}
		st.log("%s: failed: %v", addr, err)
		return nil, err
	}
}

func dialProxyConn(ctx context.Context, dialer *net.Dialer, pc *ProxyConfig) (net.Conn, error) {
	conn, err := dialer.DialContext(ctx, "tcp", pc.URL.Host)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProxyConnect, unwrapURLError(err))
	}
	if pc.URL.Scheme != "https" {
		return conn, nil
	}
	tlsConn := tls.Client(conn, &tls.Config{ServerName: pc.URL.Hostname(), MinVersion: tls.VersionTLS12})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("%w: TLS to proxy: %v", ErrProxyConnect, err)
	}
	return tlsConn, nil
}

// connectOnce runs the CONNECT exchange on conn under the context's deadline.
func connectOnce(ctx context.Context, conn net.Conn, pc *ProxyConfig, user, pass, addr string, st *proxyState) (net.Conn, error) {
	deadline := time.Now().Add(proxyHandshakeTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	tunnel, err := connectRounds(conn, pc, user, pass, addr, st)
	if !stop() {
		// The context ended while the exchange ran; the deadline now poisons conn.
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return tunnel, nil
}

func connectRounds(conn net.Conn, pc *ProxyConfig, user, pass, addr string, st *proxyState) (net.Conn, error) {
	br := bufio.NewReader(conn)
	for round := 1; round <= maxProxyRounds; round++ {
		req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: addr}, Host: addr, Header: make(http.Header)}
		sending := "no credentials"
		if st.authz != "" {
			req.Header.Set("Proxy-Authorization", st.authz)
			sending, _, _ = strings.Cut(st.authz, " ")
		}
		st.log("%s: CONNECT round %d, sending %s", addr, round, sending)
		if err := req.Write(conn); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrProxyConnect, unwrapURLError(err))
		}
		resp, err := http.ReadResponse(br, req)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrProxyConnect, unwrapURLError(err))
		}
		st.log("%s: proxy replied %s", addr, resp.Status)
		if resp.StatusCode/100 == 2 {
			if br.Buffered() > 0 {
				return &bufferedConn{Conn: conn, r: br}, nil
			}
			return conn, nil
		}
		status, closing := resp.Status, resp.Close
		challenges := parseChallenges(resp.Header.Values("Proxy-Authenticate"))
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if resp.StatusCode != http.StatusProxyAuthRequired {
			return nil, fmt.Errorf("%w: %s", ErrProxyConnect, status)
		}

		st.log("%s: proxy offers %s", addr, offeredSchemes(challenges))
		if st.auth == nil {
			if st.auth, err = selectProxyAuth(pc, user, pass, challenges); err != nil {
				st.log("%s: cannot answer the challenge: %v", addr, err)
				return nil, err
			}
			st.log("%s: answering with %s", addr, st.auth.scheme())
		}
		if st.authz, err = st.auth.respond(challenges[st.auth.scheme()]); err != nil {
			st.log("%s: %s authentication failed: %v", addr, st.auth.scheme(), err)
			return nil, err
		}
		if closing {
			if st.auth.connectionBound() {
				return nil, fmt.Errorf("%w: proxy closed the connection during %s authentication", ErrProxyConnect, st.auth.scheme())
			}
			return nil, errProxyReconnect
		}
	}
	return nil, ErrProxyAuthFailed
}

// unwrapURLError drops the URL from a url.Error so a proxy address carrying
// credentials never reaches an error message.
func unwrapURLError(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}

// bufferedConn serves bytes already read past the CONNECT response first.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
