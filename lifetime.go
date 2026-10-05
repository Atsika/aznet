package aznet

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"
)

// ErrBootstrapDurationUnsupported means a custom driver cannot issue bootstrap
// credentials with an independent duration. Its default ConnectionString still works.
var ErrBootstrapDurationUnsupported = errors.New("driver does not support an independent bootstrap duration")

// BootstrapTokenIssuer is an optional Driver capability. It issues credentials
// for one bootstrap URL without changing the driver's session policy.
type BootstrapTokenIssuer interface {
	CreateBootstrapTokensFor(time.Duration) (handshake, token string, err error)
}

// ConnectionStringFor issues bootstrap credentials valid for duration from their
// issuance time. It does not mutate listener/session configuration or renew any
// existing credential. Azure SAS timestamps have whole-second precision.
func (l *Listener) ConnectionStringFor(duration time.Duration) (string, error) {
	if err := validateCredentialDuration(duration); err != nil {
		return "", err
	}
	issuer, ok := l.driver.(BootstrapTokenIssuer)
	if !ok {
		return "", ErrBootstrapDurationUnsupported
	}
	h, t, err := issuer.CreateBootstrapTokensFor(duration)
	if err != nil {
		return "", err
	}
	return l.ep.BuildConnURL(l.cfg, h, t), nil
}

func (d *metricsDriver) CreateBootstrapTokensFor(duration time.Duration) (string, string, error) {
	issuer, ok := d.Driver.(BootstrapTokenIssuer)
	if !ok {
		return "", "", ErrBootstrapDurationUnsupported
	}
	return issuer.CreateBootstrapTokensFor(duration)
}

// SessionExpiry returns the exact earliest required session-token expiration.
// False means unknown (for example, a custom driver without issuance metadata).
// Expiration is informational: it is not a liveness promise or a close timer.
func (c *Conn) SessionExpiry() (time.Time, bool) {
	return c.sessionExpiry, !c.sessionExpiry.IsZero()
}

// GetSessionExpiry reads the optional connection capability without requiring
// callers to inspect credentials. Non-aznet connections can implement it too.
func GetSessionExpiry(conn net.Conn) (time.Time, bool) {
	type provider interface{ SessionExpiry() (time.Time, bool) }
	if p, ok := conn.(provider); ok {
		return p.SessionExpiry()
	}
	return time.Time{}, false
}

func validateCredentialDuration(duration time.Duration) error {
	if duration < time.Second {
		return fmt.Errorf("%w: credential duration must be at least one second", ErrInvalidConfig)
	}
	return nil
}

// Read the signed timestamps inside the adapter, so reported precision exactly
// matches the SDK's issued SAS rather than an unsent high-resolution estimate.
func issuedSessionTokens(req, res string) (SessionTokens, error) {
	var earliest time.Time
	for _, token := range []string{req, res} {
		values, err := url.ParseQuery(token)
		if err != nil {
			return SessionTokens{}, fmt.Errorf("%w: invalid issued token encoding", ErrSASGenerationFailed)
		}
		end, err := time.Parse(time.RFC3339, values.Get("se"))
		if err != nil {
			return SessionTokens{}, fmt.Errorf("%w: missing or invalid issued expiry", ErrSASGenerationFailed)
		}
		if earliest.IsZero() || end.Before(earliest) {
			earliest = end
		}
	}
	return SessionTokens{Req: req, Res: res, ExpiresAt: earliest.UTC()}, nil
}
