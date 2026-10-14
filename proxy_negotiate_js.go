//go:build js

package aznet

// The browser-style host owns networking and cannot run these exchanges.

func newNegotiateAuth(*ProxyConfig, string, string) (proxyAuthenticator, error) {
	return nil, ErrProxyAuthUnsupported
}

func newAmbientNTLMAuth() (proxyAuthenticator, error) { return nil, ErrProxyAuthUnsupported }
