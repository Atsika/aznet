//go:build !windows && !js

package aznet

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/jcmturner/gokrb5/v8/client"
	"github.com/jcmturner/gokrb5/v8/config"
	"github.com/jcmturner/gokrb5/v8/credentials"
	"github.com/jcmturner/gokrb5/v8/spnego"
)

// kerberosNegotiate answers a Negotiate challenge with a Kerberos service
// ticket for the proxy. The exchange is a single token; there is no NTLM
// fallback inside it, so ProxyAuto falls back to NTLM instead.
type kerberosNegotiate struct {
	cl   *client.Client
	spn  string
	sent bool
}

// newNegotiateAuth logs in with user and pass when both are given, and
// otherwise uses the ticket cache named by KRB5CCNAME.
func newNegotiateAuth(pc *ProxyConfig, user, pass string) (proxyAuthenticator, error) {
	var cl *client.Client
	var err error
	if user != "" && pass != "" {
		cl, err = kerberosPasswordClient(user, pass)
	} else {
		cl, err = kerberosCacheClient()
	}
	if err != nil {
		return nil, err
	}
	return &kerberosNegotiate{cl: cl, spn: proxySPN(pc)}, nil
}

func (*kerberosNegotiate) scheme() string        { return "negotiate" }
func (*kerberosNegotiate) connectionBound() bool { return false }

func (a *kerberosNegotiate) respond(string) (string, error) {
	if a.sent {
		return "", ErrProxyAuthFailed // the proxy rejected the ticket
	}
	a.sent = true
	var header string
	err := guardKerberos(func() error {
		s := spnego.SPNEGOClient(a.cl, a.spn)
		if err := s.AcquireCred(); err != nil {
			return fmt.Errorf("%w: kerberos login: %v", ErrProxyAuthFailed, err)
		}
		token, err := s.InitSecContext()
		if err != nil {
			return fmt.Errorf("%w: kerberos ticket for %s: %v", ErrProxyAuthFailed, a.spn, err)
		}
		raw, err := token.Marshal()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrProxyAuthFailed, err)
		}
		header = "Negotiate " + base64.StdEncoding.EncodeToString(raw)
		return nil
	})
	return header, err
}

// guardKerberos turns a panic in the Kerberos library, which parses cache
// files and KDC replies without bounds checks, into an authentication error
// instead of ending the process.
func guardKerberos(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: kerberos: malformed data", ErrProxyAuthFailed)
		}
	}()
	return fn()
}

// newAmbientNTLMAuth: there is no logged-on identity to borrow outside Windows.
func newAmbientNTLMAuth() (proxyAuthenticator, error) { return nil, ErrProxyAuthUnsupported }

// passwordClients keeps logged-in clients across connections, so reconnects
// reuse the ticket-granting ticket (which gokrb5 renews) instead of asking the
// KDC again.
var passwordClients sync.Map // "name@realm\x00password" -> *client.Client

func kerberosPasswordClient(user, pass string) (*client.Client, error) {
	name, realm := splitKerberosUser(user)
	cfg, err := loadKerberosConfig(realm)
	if err != nil {
		return nil, err
	}
	if realm == "" {
		realm = cfg.LibDefaults.DefaultRealm
	}
	if realm == "" {
		return nil, fmt.Errorf("%w: no Kerberos realm: use user@REALM or set default_realm in krb5.conf", ErrProxyAuthRequired)
	}
	key := name + "@" + realm + "\x00" + pass
	if cl, ok := passwordClients.Load(key); ok {
		return cl.(*client.Client), nil
	}
	cl := client.NewWithPassword(name, realm, pass, cfg, client.DisablePAFXFAST(true))
	err = guardKerberos(func() error {
		if err := cl.Login(); err != nil {
			return fmt.Errorf("%w: kerberos login for %s@%s: %v", ErrProxyAuthFailed, name, realm, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	passwordClients.Store(key, cl)
	return cl, nil
}

// kerberosCacheClient reads the ticket cache afresh each time, so tickets
// renewed outside the process are picked up.
func kerberosCacheClient() (*client.Client, error) {
	path, err := kerberosCachePath()
	if err != nil {
		return nil, err
	}
	var cl *client.Client
	err = guardKerberos(func() error {
		cc, err := credentials.LoadCCache(path)
		if err != nil {
			return fmt.Errorf("%w: no usable Kerberos ticket cache: %v", ErrProxyAuthRequired, err)
		}
		cfg, err := loadKerberosConfig("")
		if err != nil {
			return err
		}
		if cl, err = client.NewFromCCache(cc, cfg, client.DisablePAFXFAST(true)); err != nil {
			return fmt.Errorf("%w: kerberos ticket cache: %v", ErrProxyAuthRequired, err)
		}
		return nil
	})
	if errors.Is(err, ErrProxyAuthFailed) {
		// A cache that cannot be parsed is as good as none.
		err = fmt.Errorf("%w: unreadable Kerberos ticket cache", ErrProxyAuthRequired)
	}
	return cl, err
}

// kerberosCachePath resolves KRB5CCNAME. Only file caches can be read: the
// keyring, KCM and macOS API caches are held by system services.
func kerberosCachePath() (string, error) {
	name := os.Getenv("KRB5CCNAME")
	if name == "" {
		return "/tmp/krb5cc_" + strconv.Itoa(os.Getuid()), nil
	}
	if kind, rest, ok := strings.Cut(name, ":"); ok && len(kind) > 1 {
		if strings.EqualFold(kind, "FILE") {
			return rest, nil
		}
		return "", fmt.Errorf("%w: Kerberos cache type %s: is not readable; set KRB5CCNAME=FILE:<path>", ErrProxyAuthUnsupported, strings.ToUpper(kind))
	}
	return name, nil
}

// splitKerberosUser accepts user, user@REALM and DOMAIN\user. The domain of
// the last form is a NetBIOS name, not a realm, so it is dropped.
func splitKerberosUser(user string) (name, realm string) {
	if _, u, ok := strings.Cut(user, `\`); ok {
		user = u
	}
	name, realm, _ = strings.Cut(user, "@")
	return name, strings.ToUpper(realm)
}

// loadKerberosConfig reads krb5.conf (KRB5_CONFIG, else /etc/krb5.conf) and,
// when there is none, locates the KDC through DNS.
func loadKerberosConfig(realm string) (*config.Config, error) {
	path := os.Getenv("KRB5_CONFIG")
	if path == "" {
		path = "/etc/krb5.conf"
	}
	if first, _, ok := strings.Cut(path, ":"); ok {
		path = first
	}
	if cfg, err := config.Load(path); err == nil {
		return cfg, nil
	}
	conf := "[libdefaults]\n dns_lookup_kdc = true\n"
	if realm != "" {
		conf += " default_realm = " + realm + "\n"
	}
	cfg, err := config.NewFromString(conf)
	if err != nil {
		return nil, errors.New("kerberos configuration")
	}
	return cfg, nil
}
