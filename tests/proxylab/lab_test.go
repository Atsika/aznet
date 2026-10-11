package proxylab

// End-to-end proxy authentication tests against the corporate-style lab in
// docker-compose.yml. They dial through real Squid proxies backed by a Samba
// Active Directory domain, to an Azurite emulator behind them.
//
//	cd tests/proxylab && docker compose up -d --build
//	AZNET_PROXYLAB=1 go test -v -count=1 ./tests/proxylab/
//
// AZNET_PROXYLAB_HOST=<address> points the tests at a lab running on another
// machine (its published ports must be reachable).
//
// Without AZNET_PROXYLAB the tests are skipped, so `go test ./...` stays
// offline.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/atsika/aznet"
)

const (
	password = "Passw0rd!2024"

	account = "devstoreaccount1:Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq%2FK1SZFPTOtr%2FKBHBeksoGMGw%3D%3D"
)

// By default the tests run on the host that runs docker compose, reaching the
// proxies through their published ports. With AZNET_PROXYLAB_INSIDE=1 they run
// inside the lab network (docker compose run runner), reaching everything by
// service name, which is how a Linux machine on a corporate network sees it.
var (
	inside = os.Getenv("AZNET_PROXYLAB_INSIDE") != ""

	basicProxy, ntlmProxy, negotiateProxy, allProxy string
	labHost                                         string
	azuriteURL                                      string // where the listener reaches Azurite
	krb5ConfFile                                    string
)

func init() {
	if inside {
		labHost = "azurite"
		basicProxy, ntlmProxy = "http://proxy-basic:3128", "http://proxy-ntlm:3128"
		negotiateProxy, allProxy = "http://proxy-negotiate:3128", "http://proxy-all:3128"
		azuriteURL = "http://" + account + "@azurite:10000/devstoreaccount1"
		krb5ConfFile = "krb5-inside.conf"
		return
	}
	labHost = "localhost"
	if h := os.Getenv("AZNET_PROXYLAB_HOST"); h != "" {
		labHost = h // the lab runs on another machine
	}
	basicProxy, ntlmProxy = "http://"+labHost+":3128", "http://"+labHost+":3129"
	negotiateProxy, allProxy = "http://"+labHost+":3130", "http://"+labHost+":3131"
	azuriteURL = "http://" + account + "@" + labHost + ":10000/devstoreaccount1"
	krb5ConfFile = "krb5.conf"
}

func TestMain(m *testing.M) {
	if os.Getenv("AZNET_PROXYLAB") == "" {
		fmt.Println("skipped: set AZNET_PROXYLAB=1 with the lab running (tests/proxylab/docker-compose.yml)")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

var (
	labOnce sync.Once
	labConn string
	labErr  error
)

// startEcho runs one echo listener on Azurite for every scenario and returns
// the connection string clients use.
func startEcho(t *testing.T) string {
	t.Helper()
	labOnce.Do(func() {
		ln, err := aznet.Listen("azblob", azuriteURL)
		if err != nil {
			labErr = err
			return
		}
		labConn, labErr = ln.(*aznet.Listener).ConnectionString()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() { defer c.Close(); io.Copy(c, c) }()
			}
		}()
	})
	if labErr != nil {
		t.Fatalf("listener (is Azurite up?): %v", labErr)
	}
	// Proxies reach Azurite by its service name, not through the host port.
	return strings.Replace(labConn, labHost+":10000", "azurite:10000", 1)
}

// kinit logs user in with a Heimdal/MIT kinit into a file cache and returns
// the cache name for KRB5CCNAME.
func kinit(t *testing.T, user string) string {
	t.Helper()
	cache := "FILE:" + filepath.Join(t.TempDir(), "krb5cc_lab")
	return kinitTo(t, user, cache)
}

// kinitTo logs in with the platform's kinit: Heimdal on macOS takes the
// password from --password-file, MIT reads it from a piped stdin. An empty
// cache leaves KRB5CCNAME unset, so kinit writes the platform default.
func kinitTo(t *testing.T, user, cache string) string {
	t.Helper()
	args := []string{user}
	if runtime.GOOS == "darwin" {
		args = []string{"--password-file=STDIN", user}
	}
	cmd := exec.Command("kinit", args...)
	cmd.Env = append(envWithout("KRB5CCNAME"), "KRB5_CONFIG="+krb5Conf(t))
	if cache != "" {
		cmd.Env = append(cmd.Env, "KRB5CCNAME="+cache)
	}
	cmd.Stdin = strings.NewReader(password + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kinit %s: %v\n%s", user, err, out)
	}
	return cache
}

func envWithout(name string) []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	return out
}

func krb5Conf(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(krb5ConfFile)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type scenario struct {
	name  string
	proxy string
	auth  aznet.ProxyAuth
	user  string
	pass  string
	spn   string // Kerberos service name; defaults to the proxy's own
	cache string // "kinit:<user>" logs in first, "none" points KRB5CCNAME at nothing
	// kerberos marks scenarios that need Kerberos realm access from the
	// client. On Windows that means a machine that can find the lab KDC
	// (joined to the domain, or configured with ksetup); they are skipped
	// there unless AZNET_PROXYLAB_KDC=1.
	kerberos bool
	// ambient marks scenarios that use the identity of the running process:
	// run them with AZNET_PROXYLAB_AMBIENT=1 under
	// `runas /netonly /user:CORP\alice` on Windows.
	ambient bool
	// noKerberos points KRB5_CONFIG at nothing, as on a machine with no
	// Kerberos setup, so a bare user name has no realm to log in to.
	noKerberos bool
	// want is the sentinel the dial must fail with, or nil for success.
	want error
	// wantWindows replaces want on Windows, where a missing password means
	// "authenticate as the logged-on user" rather than "no credentials".
	wantWindows error
	// logHas lists strings the proxy log must contain.
	logHas []string
}

func TestProxyLab(t *testing.T) {
	scenarios := []scenario{
		// Basic, checked against Active Directory.
		{name: "basic/explicit", proxy: basicProxy, auth: aznet.ProxyBasic, user: "alice", pass: password,
			logHas: []string{"sending Basic", "tunnel established"}},
		{name: "basic/domain-user", proxy: basicProxy, auth: aznet.ProxyBasic, user: `CORP\alice`, pass: password},
		{name: "basic/auto", proxy: basicProxy, auth: aznet.ProxyAuto, user: "bob", pass: password,
			logHas: []string{"proxy offers basic", "answering with basic"}},
		{name: "basic/wrong-password", proxy: basicProxy, auth: aznet.ProxyBasic, user: "alice", pass: "nope", want: aznet.ErrProxyAuthFailed},
		{name: "basic/disabled-account", proxy: basicProxy, auth: aznet.ProxyBasic, user: "carol", pass: password, want: aznet.ErrProxyAuthFailed},
		{name: "basic/no-credentials", proxy: basicProxy, auth: aznet.ProxyAuto, want: aznet.ErrProxyAuthRequired},

		// NTLM, validated by Samba's ntlm_auth.
		{name: "ntlm/explicit", proxy: ntlmProxy, auth: aznet.ProxyNTLM, user: "alice", pass: password,
			logHas: []string{"answering with ntlm", "tunnel established"}},
		{name: "ntlm/domain-user", proxy: ntlmProxy, auth: aznet.ProxyNTLM, user: `CORP\alice`, pass: password},
		{name: "ntlm/upn-user", proxy: ntlmProxy, auth: aznet.ProxyNTLM, user: "alice@corp.test", pass: password},
		{name: "ntlm/auto", proxy: ntlmProxy, auth: aznet.ProxyAuto, user: "bob", pass: password,
			logHas: []string{"proxy offers ntlm", "answering with ntlm"}},
		{name: "ntlm/wrong-password", proxy: ntlmProxy, auth: aznet.ProxyNTLM, user: "alice", pass: "nope", want: aznet.ErrProxyAuthFailed},
		{name: "ntlm/disabled-account", proxy: ntlmProxy, auth: aznet.ProxyNTLM, user: "carol", pass: password, want: aznet.ErrProxyAuthFailed},
		{name: "ntlm/no-credentials", proxy: ntlmProxy, auth: aznet.ProxyAuto, want: aznet.ErrProxyAuthRequired,
			wantWindows: aznet.ErrProxyAuthFailed}, // the logged-on user is not in the lab domain

		// Negotiate, with real Kerberos tickets from the lab KDC.
		{name: "negotiate/ticket-cache", kerberos: true, proxy: negotiateProxy, auth: aznet.ProxyNegotiate, cache: "kinit:alice@CORP.TEST",
			spn: "HTTP/proxy-negotiate.corp.test", logHas: []string{"answering with negotiate", "tunnel established"}},
		{name: "negotiate/password-login", kerberos: true, proxy: negotiateProxy, auth: aznet.ProxyNegotiate, user: "alice@CORP.TEST", pass: password,
			spn: "HTTP/proxy-negotiate.corp.test"},
		{name: "negotiate/auto", kerberos: true, proxy: negotiateProxy, auth: aznet.ProxyAuto, cache: "kinit:bob@CORP.TEST",
			spn: "HTTP/proxy-negotiate.corp.test", logHas: []string{"proxy offers negotiate", "answering with negotiate"}},
		{name: "negotiate/no-ticket-cache", kerberos: true, proxy: negotiateProxy, auth: aznet.ProxyNegotiate, cache: "none",
			spn: "HTTP/proxy-negotiate.corp.test", want: aznet.ErrProxyAuthRequired},
		{name: "negotiate/default-cache-path", kerberos: true, proxy: negotiateProxy, auth: aznet.ProxyNegotiate, cache: "default:alice@CORP.TEST",
			spn: "HTTP/proxy-negotiate.corp.test", logHas: []string{"answering with negotiate"}},
		{name: "negotiate/wrong-password", kerberos: true, proxy: negotiateProxy, auth: aznet.ProxyNegotiate, user: "alice@CORP.TEST", pass: "nope",
			spn: "HTTP/proxy-negotiate.corp.test", want: aznet.ErrProxyAuthFailed},
		{name: "negotiate/unknown-service-name", kerberos: true, proxy: negotiateProxy, auth: aznet.ProxyNegotiate, cache: "kinit:alice@CORP.TEST",
			spn: "HTTP/nobody.corp.test", want: aznet.ErrProxyAuthFailed},

		// A company proxy offering everything: Negotiate, NTLM, Basic.
		{name: "all/auto-prefers-negotiate", kerberos: true, proxy: allProxy, auth: aznet.ProxyAuto, cache: "kinit:alice@CORP.TEST",
			spn: "HTTP/proxy-all.corp.test", logHas: []string{"proxy offers basic, negotiate, ntlm", "answering with negotiate"}},
		{name: "all/auto-password-uses-kerberos", kerberos: true, proxy: allProxy, auth: aznet.ProxyAuto, user: "alice@CORP.TEST", pass: password,
			spn: "HTTP/proxy-all.corp.test", logHas: []string{"answering with negotiate"}},
		{name: "all/auto-ntlm-when-kerberos-unconfigured", kerberos: true, proxy: allProxy, auth: aznet.ProxyAuto, user: "alice", pass: password,
			cache: "none", noKerberos: true, logHas: []string{"answering with ntlm", "tunnel established"}},
		{name: "all/auto-falls-back-to-ntlm", kerberos: true, proxy: allProxy, auth: aznet.ProxyAuto, user: "alice@CORP.TEST", pass: password,
			spn: "HTTP/nobody.corp.test", logHas: []string{"falling back to ntlm", "tunnel established"}},
		{name: "all/explicit-ntlm", proxy: allProxy, auth: aznet.ProxyNTLM, user: "alice", pass: password,
			logHas: []string{"answering with ntlm"}},
		{name: "all/explicit-basic", proxy: allProxy, auth: aznet.ProxyBasic, user: "alice", pass: password,
			logHas: []string{"sending Basic"}},

		// The identity of the running process (Windows SSPI; skipped elsewhere).
		{name: "ambient/ntlm-only-proxy", ambient: true, proxy: ntlmProxy, auth: aznet.ProxyNTLM,
			logHas: []string{"answering with ntlm", "tunnel established"}},
		{name: "ambient/auto-ntlm-only-proxy", ambient: true, proxy: ntlmProxy, auth: aznet.ProxyAuto,
			logHas: []string{"proxy offers ntlm", "tunnel established"}},
		{name: "ambient/negotiate", ambient: true, kerberos: true, proxy: negotiateProxy, auth: aznet.ProxyNegotiate,
			spn: "HTTP/proxy-negotiate.corp.test", logHas: []string{"answering with negotiate", "tunnel established"}},
		{name: "ambient/auto-all-proxy", ambient: true, proxy: allProxy, auth: aznet.ProxyAuto,
			spn: "HTTP/proxy-all.corp.test", logHas: []string{"tunnel established"}},

		// Scheme the proxy does not offer.
		{name: "basic-proxy/negotiate-requested", proxy: basicProxy, auth: aznet.ProxyNegotiate, cache: "none", want: aznet.ErrProxyAuthUnsupported},
		{name: "ntlm-proxy/basic-requested", proxy: ntlmProxy, auth: aznet.ProxyBasic, user: "alice", pass: password, want: aznet.ErrProxyAuthFailed},
	}

	conn := startEcho(t)
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) { run(t, conn, sc) })
	}
}

func run(t *testing.T, connString string, sc scenario) {
	switch {
	case sc.ambient && os.Getenv("AZNET_PROXYLAB_AMBIENT") == "":
		t.Skip("ambient identity: set AZNET_PROXYLAB_AMBIENT=1 (Windows, under runas /netonly)")
	case !sc.ambient && os.Getenv("AZNET_PROXYLAB_AMBIENT") != "":
		t.Skip("only ambient-identity scenarios run in AZNET_PROXYLAB_AMBIENT mode")
	case sc.ambient && runtime.GOOS != "windows":
		t.Skip("there is no logged-on identity to borrow outside Windows")
	case runtime.GOOS == "windows" && (sc.cache != "" || sc.noKerberos):
		t.Skip("exercises a Unix ticket cache or krb5.conf; Windows uses SSPI")
	case sc.kerberos && runtime.GOOS == "windows" && os.Getenv("AZNET_PROXYLAB_KDC") == "":
		t.Skip("needs a Windows client that can reach the lab KDC: set AZNET_PROXYLAB_KDC=1 (see README)")
	}
	t.Setenv("KRB5_CONFIG", krb5Conf(t))
	if sc.noKerberos {
		t.Setenv("KRB5_CONFIG", filepath.Join(t.TempDir(), "absent.conf"))
	}
	switch {
	case sc.cache == "none":
		t.Setenv("KRB5CCNAME", "FILE:"+filepath.Join(t.TempDir(), "absent"))
	case strings.HasPrefix(sc.cache, "kinit:"):
		t.Setenv("KRB5CCNAME", kinit(t, strings.TrimPrefix(sc.cache, "kinit:")))
	case strings.HasPrefix(sc.cache, "default:"):
		// No KRB5CCNAME: the agent must find the platform's default file cache.
		if runtime.GOOS != "linux" {
			t.Skip("the default ticket cache is a readable file only on Linux")
		}
		t.Setenv("KRB5CCNAME", "")
		kinitTo(t, strings.TrimPrefix(sc.cache, "default:"), "")
	}

	var logBuf bytes.Buffer
	var mu sync.Mutex
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(&logBuf, format+"\n", args...)
	}
	u, err := url.Parse(sc.proxy)
	if err != nil {
		t.Fatal(err)
	}
	// The scenario's SPN goes in through a resolver, the way ProxyBlob does.
	pc := &aznet.ProxyConfig{URL: u, Auth: sc.auth, User: sc.user, Pass: sc.pass, SPN: sc.spn}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	c, err := aznet.Dial("azblob", connString,
		aznet.WithContext(ctx),
		aznet.WithProxyFunc(func(context.Context, *url.URL) (*aznet.ProxyConfig, error) { return pc, nil }),
		aznet.WithProxyLog(logf),
	)
	mu.Lock()
	logText := logBuf.String()
	mu.Unlock()
	t.Logf("proxy log:\n%s", logText)

	for _, secret := range []string{password, "nope"} {
		if strings.Contains(logText, secret) {
			t.Errorf("the log contains the password %q", secret)
		}
	}
	for _, want := range sc.logHas {
		if !strings.Contains(logText, want) {
			t.Errorf("log lacks %q", want)
		}
	}

	if runtime.GOOS == "windows" && sc.wantWindows != nil {
		sc.want = sc.wantWindows
	}
	if sc.want != nil {
		// A rejected login is permanent: the SDK must not repeat it, since
		// against a directory every attempt counts toward locking the account.
		if n := strings.Count(logText, ": via "); n != 1 {
			t.Errorf("the proxy was tried %d times, want 1", n)
		}
		if err == nil {
			c.Close()
			t.Fatalf("dial succeeded, want %v", sc.want)
		}
		if !errors.Is(err, sc.want) && !strings.Contains(err.Error(), sc.want.Error()) {
			t.Fatalf("dial: %v, want %v", err, sc.want)
		}
		return
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if err := echo(c); err != nil {
		t.Fatalf("echo through the tunnel: %v", err)
	}
}

func echo(c net.Conn) error {
	c.SetDeadline(time.Now().Add(30 * time.Second))
	msg := []byte("hello through the corporate proxy")
	if _, err := c.Write(msg); err != nil {
		return err
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(c, got); err != nil {
		return err
	}
	if !bytes.Equal(got, msg) {
		return fmt.Errorf("echoed %q", got)
	}
	return nil
}

// TestDirect checks that the proxy code leaves a direct connection alone: no
// proxy option at all, and a resolver that answers "no proxy".
func TestDirect(t *testing.T) {
	startEcho(t)
	direct := labConn // the listener's own address, reachable without a proxy
	for _, tc := range []struct {
		name string
		opts []aznet.Option
		log  string
	}{
		{name: "no proxy option"},
		{name: "resolver says direct", opts: []aznet.Option{
			aznet.WithProxyFunc(func(context.Context, *url.URL) (*aznet.ProxyConfig, error) { return nil, nil }),
		}, log: "direct connection"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var logBuf bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			opts := append(tc.opts, aznet.WithContext(ctx), aznet.WithProxyLog(func(f string, a ...any) {
				mu.Lock()
				defer mu.Unlock()
				fmt.Fprintf(&logBuf, f+"\n", a...)
			}))
			c, err := aznet.Dial("azblob", direct, opts...)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer c.Close()
			if err := echo(c); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if tc.log != "" && !strings.Contains(logBuf.String(), tc.log) {
				t.Errorf("log lacks %q:\n%s", tc.log, logBuf.String())
			}
			if strings.Contains(logBuf.String(), "via ") {
				t.Errorf("went through a proxy:\n%s", logBuf.String())
			}
		})
	}
}
