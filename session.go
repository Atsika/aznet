package aznet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

// AcceptError identifies the failed setup operation and preserves the underlying
// error (including SDK response details). Temporary reports whether a caller may
// retry Accept; Accept itself only retries empty polls.
type AcceptError struct {
	Op  string
	Err error
}

func (e *AcceptError) Error() string { return "aznet accept " + e.Op + ": " + e.Err.Error() }
func (e *AcceptError) Unwrap() error { return e.Err }
func (e *AcceptError) Timeout() bool {
	var n net.Error
	return errors.As(e.Err, &n) && n.Timeout()
}
func (e *AcceptError) Temporary() bool { return transient(e.Err) }

func transient(err error) bool {
	if errors.Is(err, ErrResourceBeingDeleted) {
		return true
	}
	var r *azcore.ResponseError
	if errors.As(err, &r) {
		return r.StatusCode == 408 || r.StatusCode == 429 || r.StatusCode >= 500
	}
	var n net.Error
	return errors.As(err, &n) && (n.Timeout() || n.Temporary())
}

const cleanupTimeout = 2 * time.Second
const cleanupAttempts = 3

// missingDelete makes deletion idempotent without concealing authorization,
// network or other service failures.
func missingDelete(err error) error {
	// errors.As alone only examines the first matching SDK error. A custom
	// driver may join failures from several resources; all branches must be
	// absent before the combined deletion can be reported as successful.
	if deletionAbsent(err) {
		return nil
	}
	return err
}

// cleanup bounds both attempts and elapsed time, including a custom driver that
// ignores cancellation. A timed-out call is never overlapped with another retry.
func cleanup(op string, f func(context.Context) error) error {
	return boundedCleanup(op, cleanupAttempts, func(ctx context.Context) error {
		return missingDelete(f(ctx))
	})
}

// A partial transport is acquired once and closed once. Unlike storage deletion,
// Transport.Close does not promise retry safety after a failed attempt.
func disposeTransport(transport Transport) error {
	return boundedCleanup("close transport", 1, func(context.Context) error { return transport.Close() })
}

func boundedCleanup(op string, attempts int, f func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		var err error
		for attempt := 0; attempt < attempts; attempt++ {
			err = f(ctx)
			if err == nil || !transient(err) || ctx.Err() != nil {
				break
			}
			if attempt+1 < attempts {
				select {
				case <-ctx.Done():
					result <- ctx.Err()
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s: %w", op, ctx.Err())
	}
}

// sessionOwner is installed before the first acquisition and transferred to the
// accepted connection. Initiators never own server-created storage resources.
type sessionOwner struct {
	driver        Driver
	id            string
	once          sync.Once
	tokenOnce     sync.Once
	err, tokenErr error
}

func (s *sessionOwner) deleteToken() error {
	s.tokenOnce.Do(func() {
		s.tokenErr = cleanup("delete token "+s.id, func(ctx context.Context) error { return s.driver.DeleteToken(ctx, s.id) })
	})
	return s.tokenErr
}
func (s *sessionOwner) close() error {
	s.once.Do(func() {
		s.err = errors.Join(s.deleteToken(), cleanup("cleanup session "+s.id, func(ctx context.Context) error { return s.driver.CleanupSession(ctx, s.id) }))
	})
	return s.err
}

func deletionAbsent(err error) bool {
	if err == nil {
		return true
	}
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		children := e.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !deletionAbsent(child) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		if child := e.Unwrap(); child != nil {
			return deletionAbsent(child)
		}
	}
	var response *azcore.ResponseError
	return errors.As(err, &response) && response.StatusCode == 404
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
