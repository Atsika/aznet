package proxylab

// End-to-end proxy authentication tests against the corporate-style lab in
// docker-compose.yml. They dial through real Squid proxies backed by a Samba
// Active Directory domain, to an Azurite emulator behind them.
//
//	cd tests/proxylab && docker compose up -d --build
//	AZNET_PROXYLAB=1 go test -v -count=1 ./tests/proxylab/
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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/atsika/aznet"
)

const (
	password = "Passw0rd!2024"

	basicProxy     = "http://localhost:3128"
	ntlmProxy      = "http://localhost:3129"
	negotiateProxy = "http://localhost:3130"
	allProxy       = "http://localhost:3131"

	// Azurite as the proxies see it; the listener reaches it as localhost.
	azuriteURL = "http://devstoreaccount1:Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq%2FK1SZFPTOtr%2FKBHBeksoGMGw%3D%3D@localhost:10000/devstoreaccount1"
)

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
	return strings.Replace(labConn, "localhost:10000", "azurite:10000", 1)
}

// kinit logs user in with a Heimdal/MIT kinit into a file cache and returns
// the cache name for KRB5CCNAME.
func kinit(t *testing.T, user string) string {
	t.Helper()
	cache := "FILE:" + filepath.Join(t.TempDir(), "krb5cc_lab")
	cmd := exec.Command("kinit", "--password-file=STDIN", user)
	cmd.Env = append(os.Environ(), "KRB5CCNAME="+cache, "KRB5_CONFIG="+krb5Conf(t))
	cmd.Stdin = strings.NewReader(password + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kinit %s: %v\n%s", user, err, out)
	}
	return cache
}

func krb5Conf(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("krb5.conf")
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
	// noKerberos points KRB5_CONFIG at nothing, as on a machine with no
	// Kerberos setup, so a bare user name has no realm to log in to.
	noKerberos bool
	// want is the sentinel the dial must fail with, or nil for success.
	want error
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
		{name: "ntlm/no-credentials", proxy: ntlmProxy, auth: aznet.ProxyAuto, want: aznet.ErrProxyAuthRequired},

		// Negotiate, with real Kerberos tickets from the lab KDC.
		{name: "negotiate/ticket-cache", proxy: negotiateProxy, auth: aznet.ProxyNegotiate, cache: "kinit:alice@CORP.TEST",
			spn: "HTTP/proxy-negotiate.corp.test", logHas: []string{"answering with negotiate", "tunnel established"}},
		{name: "negotiate/password-login", proxy: negotiateProxy, auth: aznet.ProxyNegotiate, user: "alice@CORP.TEST", pass: password,
			spn: "HTTP/proxy-negotiate.corp.test"},
		{name: "negotiate/auto", proxy: negotiateProxy, auth: aznet.ProxyAuto, cache: "kinit:bob@CORP.TEST",
			spn: "HTTP/proxy-negotiate.corp.test", logHas: []string{"proxy offers negotiate", "answering with negotiate"}},
		{name: "negotiate/no-ticket-cache", proxy: negotiateProxy, auth: aznet.ProxyNegotiate, cache: "none",
			spn: "HTTP/proxy-negotiate.corp.test", want: aznet.ErrProxyAuthRequired},
		{name: "negotiate/wrong-password", proxy: negotiateProxy, auth: aznet.ProxyNegotiate, user: "alice@CORP.TEST", pass: "nope",
			spn: "HTTP/proxy-negotiate.corp.test", want: aznet.ErrProxyAuthFailed},
		{name: "negotiate/unknown-service-name", proxy: negotiateProxy, auth: aznet.ProxyNegotiate, cache: "kinit:alice@CORP.TEST",
			spn: "HTTP/nobody.corp.test", want: aznet.ErrProxyAuthFailed},

		// A company proxy offering everything: Negotiate, NTLM, Basic.
		{name: "all/auto-prefers-negotiate", proxy: allProxy, auth: aznet.ProxyAuto, cache: "kinit:alice@CORP.TEST",
			spn: "HTTP/proxy-all.corp.test", logHas: []string{"proxy offers basic, negotiate, ntlm", "answering with negotiate"}},
		{name: "all/auto-password-uses-kerberos", proxy: allProxy, auth: aznet.ProxyAuto, user: "alice@CORP.TEST", pass: password,
			spn: "HTTP/proxy-all.corp.test", logHas: []string{"answering with negotiate"}},
		{name: "all/auto-ntlm-when-kerberos-unconfigured", proxy: allProxy, auth: aznet.ProxyAuto, user: "alice", pass: password,
			cache: "none", noKerberos: true, logHas: []string{"answering with ntlm", "tunnel established"}},
		{name: "all/auto-falls-back-to-ntlm", proxy: allProxy, auth: aznet.ProxyAuto, user: "alice@CORP.TEST", pass: password,
			spn: "HTTP/nobody.corp.test", logHas: []string{"falling back to ntlm", "tunnel established"}},
		{name: "all/explicit-ntlm", proxy: allProxy, auth: aznet.ProxyNTLM, user: "alice", pass: password,
			logHas: []string{"answering with ntlm"}},
		{name: "all/explicit-basic", proxy: allProxy, auth: aznet.ProxyBasic, user: "alice", pass: password,
			logHas: []string{"sending Basic"}},

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
	t.Setenv("KRB5_CONFIG", krb5Conf(t))
	if sc.noKerberos {
		t.Setenv("KRB5_CONFIG", filepath.Join(t.TempDir(), "absent.conf"))
	}
	switch {
	case sc.cache == "none":
		t.Setenv("KRB5CCNAME", "FILE:"+filepath.Join(t.TempDir(), "absent"))
	case strings.HasPrefix(sc.cache, "kinit:"):
		t.Setenv("KRB5CCNAME", kinit(t, strings.TrimPrefix(sc.cache, "kinit:")))
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
