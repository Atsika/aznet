//go:build windows

package aznet

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/alexbrainman/sspi"
	"github.com/alexbrainman/sspi/negotiate"
	"github.com/alexbrainman/sspi/ntlm"
)

// sspiNegotiate runs SPNEGO through Windows SSPI, which tries Kerberos and
// falls back to NTLM itself. Empty credentials mean the logged-on user.
type sspiNegotiate struct {
	cred *sspi.Credentials
	ctx  *negotiate.ClientContext
	spn  string
}

func newNegotiateAuth(pc *ProxyConfig, user, pass string) (proxyAuthenticator, error) {
	cred, err := acquireSSPICredentials(user, pass, negotiate.AcquireCurrentUserCredentials, negotiate.AcquireUserCredentials)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProxyAuthUnsupported, err)
	}
	return &sspiNegotiate{cred: cred, spn: proxySPN(pc)}, nil
}

func (*sspiNegotiate) scheme() string        { return "negotiate" }
func (*sspiNegotiate) connectionBound() bool { return true }

func (a *sspiNegotiate) respond(challenge string) (string, error) {
	var token []byte
	if a.ctx == nil {
		ctx, out, err := negotiate.NewClientContext(a.cred, a.spn)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrProxyAuthFailed, err)
		}
		a.ctx, token = ctx, out
	} else {
		in, err := base64.StdEncoding.DecodeString(challenge)
		if err != nil || len(in) == 0 {
			return "", ErrProxyAuthFailed // a bare challenge after our token is a rejection
		}
		_, token, err = a.ctx.Update(in)
		if err != nil || len(token) == 0 {
			return "", ErrProxyAuthFailed
		}
	}
	return "Negotiate " + base64.StdEncoding.EncodeToString(token), nil
}

func (a *sspiNegotiate) release() {
	a.ctx.Release()
	a.cred.Release()
}

// sspiNTLM answers a bare NTLM challenge as the logged-on user.
type sspiNTLM struct {
	cred *sspi.Credentials
	ctx  *ntlm.ClientContext
	step int
}

func newAmbientNTLMAuth() (proxyAuthenticator, error) {
	cred, err := ntlm.AcquireCurrentUserCredentials()
	if err != nil {
		return nil, err
	}
	return &sspiNTLM{cred: cred}, nil
}

func (*sspiNTLM) scheme() string        { return "ntlm" }
func (*sspiNTLM) connectionBound() bool { return true }

func (a *sspiNTLM) respond(challenge string) (string, error) {
	a.step++
	switch a.step {
	case 1:
		ctx, msg, err := ntlm.NewClientContext(a.cred)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrProxyAuthFailed, err)
		}
		a.ctx = ctx
		return "NTLM " + base64.StdEncoding.EncodeToString(msg), nil
	case 2:
		raw, err := base64.StdEncoding.DecodeString(challenge)
		if err != nil || len(raw) == 0 {
			return "", fmt.Errorf("%w: malformed NTLM challenge", ErrProxyAuthFailed)
		}
		msg, err := a.ctx.Update(raw)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrProxyAuthFailed, err)
		}
		return "NTLM " + base64.StdEncoding.EncodeToString(msg), nil
	}
	return "", ErrProxyAuthFailed
}

func (a *sspiNTLM) release() {
	if a.ctx != nil {
		a.ctx.Release()
	}
	a.cred.Release()
}

// acquireSSPICredentials returns the logged-on user's credentials, or those
// given as DOMAIN\user or user@domain.
func acquireSSPICredentials(user, pass string, current func() (*sspi.Credentials, error), explicit func(domain, user, pass string) (*sspi.Credentials, error)) (*sspi.Credentials, error) {
	if user == "" {
		return current()
	}
	domain := ""
	if d, u, ok := strings.Cut(user, `\`); ok {
		domain, user = d, u
	}
	if user == "" {
		return nil, errors.New("empty user name")
	}
	return explicit(domain, user, pass)
}
