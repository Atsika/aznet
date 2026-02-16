package aznet

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// Endpoint represents an aznet endpoint.
type Endpoint struct {
	URL     *url.URL
	Account string
	Key     string
	IsAzure bool
}

// ParseSAS parses the handshake and token SAS tokens from the URL query.
// It returns the decoded SAS strings (without leading '?').
func (e *Endpoint) ParseSAS(cfg *Config) (string, string, error) {
	query, err := url.ParseQuery(e.URL.RawQuery)
	if err != nil {
		return "", "", ErrInvalidSASEncoding
	}

	handshakeEncoded := query.Get(cfg.handshakeEndpoint)
	tokenEncoded := query.Get(cfg.tokenEndpoint)
	if handshakeEncoded == "" || tokenEncoded == "" {
		return "", "", ErrMissingSAS
	}

	handshakeSAS, err := base64.URLEncoding.DecodeString(handshakeEncoded)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrInvalidSASEncoding, err)
	}

	tokenSAS, err := base64.URLEncoding.DecodeString(tokenEncoded)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrInvalidSASEncoding, err)
	}

	return string(handshakeSAS), string(tokenSAS), nil
}

// NewEndpoint creates a new Endpoint from a URL.
func NewEndpoint(u *url.URL) *Endpoint {
	ep := &Endpoint{
		URL: u,
	}

	hostOnly := u.Host
	if h, _, err := net.SplitHostPort(u.Host); err == nil {
		hostOnly = h
	}

	ep.IsAzure = strings.HasSuffix(strings.ToLower(hostOnly), ".core.windows.net")

	if u.User.Username() != "" {
		ep.Account = u.User.Username()
	} else if ep.IsAzure {
		// Host-based style: account.service.core.windows.net
		ep.Account = strings.Split(hostOnly, ".")[0]
	} else {
		// Path-based style: localhost/account
		path := strings.Trim(u.Path, "/")
		if path != "" {
			ep.Account = strings.Split(path, "/")[0]
		}
	}

	// Credentials fallback: URL Userinfo > Environment Variables
	if ep.Account == "" {
		ep.Account = os.Getenv("AZURE_STORAGE_ACCOUNT")
	}
	if key, ok := u.User.Password(); ok {
		ep.Key = key
	} else {
		ep.Key = os.Getenv("AZURE_STORAGE_ACCOUNT_KEY")
	}

	return ep
}

// BuildConnURL constructs the final aznet connection URL with base64 encoded SAS tokens.
func (e *Endpoint) BuildConnURL(cfg *Config, handshakeSAS, tokenSAS string) string {
	handshakeEncoded := base64.URLEncoding.EncodeToString([]byte(handshakeSAS))
	tokenEncoded := base64.URLEncoding.EncodeToString([]byte(tokenSAS))

	u := &url.URL{
		Scheme: e.URL.Scheme,
		Host:   e.URL.Host,
	}

	if !e.IsAzure {
		u.Path = "/" + e.Account
	}

	q := u.Query()
	q.Set(cfg.handshakeEndpoint, handshakeEncoded)
	q.Set(cfg.tokenEndpoint, tokenEncoded)
	u.RawQuery = q.Encode()

	return u.String()
}

// ServiceURL returns the base URL for the Azure Storage service.
func (e *Endpoint) ServiceURL() string {
	if e.IsAzure {
		return e.URL.Scheme + "://" + e.URL.Host
	}
	return e.URL.Scheme + "://" + e.URL.Host + "/" + e.Account
}

// JoinURL joins the base service URL with a resource name and optional SAS token.
func (e *Endpoint) JoinURL(resource, sas string) string {
	baseURL := e.ServiceURL()
	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}
	u := baseURL + resource
	if sas != "" {
		if !strings.HasPrefix(sas, "?") {
			u += "?"
		}
		u += sas
	}
	return u
}
