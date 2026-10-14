//go:build !windows && !js

package aznet

import (
	"bufio"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSplitKerberosUser(t *testing.T) {
	for in, want := range map[string][2]string{
		"alice":           {"alice", ""},
		"alice@corp.test": {"alice", "CORP.TEST"},
		`CORP\alice`:      {"alice", ""},
	} {
		name, realm := splitKerberosUser(in)
		if name != want[0] || realm != want[1] {
			t.Errorf("%q: %q %q, want %q", in, name, realm, want)
		}
	}
}

func TestProxySPN(t *testing.T) {
	pc := &ProxyConfig{URL: mustProxyURL(t, "http://Proxy.Corp.Test:8080")}
	if got := proxySPN(pc); got != "HTTP/proxy.corp.test" {
		t.Fatalf("SPN %q", got)
	}
	pc.SPN = "HTTP/alias.corp.test"
	if got := proxySPN(pc); got != "HTTP/alias.corp.test" {
		t.Fatalf("SPN override %q", got)
	}
}

func TestKerberosCachePath(t *testing.T) {
	t.Setenv("KRB5CCNAME", "FILE:/var/tmp/cc")
	if p, err := kerberosCachePath(); err != nil || p != "/var/tmp/cc" {
		t.Fatalf("FILE: %q, %v", p, err)
	}
	t.Setenv("KRB5CCNAME", "/var/tmp/other")
	if p, err := kerberosCachePath(); err != nil || p != "/var/tmp/other" {
		t.Fatalf("bare path %q, %v", p, err)
	}
	for _, kind := range []string{"KEYRING:persistent:1000", "KCM:", "API:abc"} {
		t.Setenv("KRB5CCNAME", kind)
		if _, err := kerberosCachePath(); !errors.Is(err, ErrProxyAuthUnsupported) {
			t.Errorf("%s: %v", kind, err)
		}
	}
	// When KRB5CCNAME is empty and the default file exists, it is returned.
	// When the file does not exist (as on macOS without kinit), an error asks
	// the user to create one.
	t.Setenv("KRB5CCNAME", "")
	p, err := kerberosCachePath()
	if err == nil {
		if !strings.HasPrefix(p, "/tmp/krb5cc_") {
			t.Fatalf("default %q", p)
		}
	} else if !errors.Is(err, ErrProxyAuthRequired) {
		t.Fatalf("default: unexpected error: %v", err)
	}
	// With a file that exists, it is always returned.
	tmp := filepath.Join(t.TempDir(), "krb5cc_test")
	os.WriteFile(tmp, []byte("test"), 0o600)
	t.Setenv("KRB5CCNAME", tmp)
	if p, err := kerberosCachePath(); err != nil || p != tmp {
		t.Fatalf("explicit file %q, %v", p, err)
	}
}

func TestNegotiateNeedsCredentialsOrACache(t *testing.T) {
	t.Setenv("KRB5CCNAME", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("KRB5_CONFIG", filepath.Join(t.TempDir(), "absent.conf"))
	pc := &ProxyConfig{URL: mustProxyURL(t, "http://proxy.corp.test:8080")}
	if _, err := newNegotiateAuth(pc, "", ""); !errors.Is(err, ErrProxyAuthRequired) {
		t.Fatalf("no cache: %v", err)
	}
	// A password with no realm anywhere cannot be sent to a KDC.
	if _, err := newNegotiateAuth(pc, "alice", "pw"); !errors.Is(err, ErrProxyAuthRequired) {
		t.Fatalf("no realm: %v", err)
	}
	empty := filepath.Join(t.TempDir(), "empty")
	os.WriteFile(empty, nil, 0o600)
	t.Setenv("KRB5CCNAME", empty)
	// gokrb5 panics on an empty cache file; that must surface as an error.
	if _, err := newNegotiateAuth(pc, "", ""); !errors.Is(err, ErrProxyAuthRequired) {
		t.Fatalf("empty cache: %v", err)
	}
	os.WriteFile(empty, []byte{5, 4, 0, 12, 0, 1}, 0o600) // truncated header
	if _, err := newNegotiateAuth(pc, "", ""); !errors.Is(err, ErrProxyAuthRequired) {
		t.Fatalf("truncated cache: %v", err)
	}
}

// When Kerberos cannot start, ProxyAuto must fall through to NTLM instead of
// failing the connection.
func TestProxyAutoFallsBackToNTLMWhenKerberosUnavailable(t *testing.T) {
	t.Setenv("KRB5CCNAME", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("KRB5_CONFIG", filepath.Join(t.TempDir(), "absent.conf"))
	var second string
	addr := startProxy(t, func(conn net.Conn, br *bufio.Reader) {
		readConnect(t, br)
		respond(conn, "407 Proxy Authentication Required", "Proxy-Authenticate: Negotiate", "Proxy-Authenticate: NTLM")
		if req := readConnect(t, br); req != nil {
			second = req.Header.Get("Proxy-Authorization")
		}
		respond(conn, "403 Forbidden")
	})
	connectThrough(t, &ProxyConfig{URL: mustProxyURL(t, addr), User: "alice", Pass: "pw"})
	if !strings.HasPrefix(second, "NTLM ") {
		t.Fatalf("answered with %q, want NTLM", second)
	}
}

func TestProxyNegotiateWithoutTicketsReportsWhy(t *testing.T) {
	t.Setenv("KRB5CCNAME", filepath.Join(t.TempDir(), "absent"))
	addr := startProxy(t, func(conn net.Conn, br *bufio.Reader) {
		readConnect(t, br)
		respond(conn, "407 Proxy Authentication Required", "Proxy-Authenticate: Negotiate")
	})
	_, err := connectThrough(t, &ProxyConfig{URL: mustProxyURL(t, addr), Auth: ProxyNegotiate})
	if !errors.Is(err, ErrProxyAuthRequired) {
		t.Fatalf("err %v", err)
	}
}

func TestProxyAmbientNTLMUnavailableOutsideWindows(t *testing.T) {
	addr := startProxy(t, func(conn net.Conn, br *bufio.Reader) {
		readConnect(t, br)
		respond(conn, "407 Proxy Authentication Required", "Proxy-Authenticate: NTLM")
	})
	_, err := connectThrough(t, &ProxyConfig{URL: mustProxyURL(t, addr)})
	if !errors.Is(err, ErrProxyAuthRequired) {
		t.Fatalf("err %v", err)
	}
}
