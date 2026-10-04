package aznet

import (
	"context"
	"errors"
	"fmt"
	"net"
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

func (e *AcceptError) Error() string   { return "aznet accept " + e.Op + ": " + e.Err.Error() }
func (e *AcceptError) Unwrap() error   { return e.Err }
func (e *AcceptError) Timeout() bool   { var n net.Error; return errors.As(e.Err, &n) && n.Timeout() }
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
	var r *azcore.ResponseError
	if errors.As(err, &r) && r.StatusCode == 404 {
		return nil
	}
	return err
}

// cleanup bounds both attempts and elapsed time, including a custom driver that
// ignores cancellation. A timed-out call is never overlapped with another retry.
func cleanup(op string, f func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		var err error
		for attempt := 0; attempt < cleanupAttempts; attempt++ {
			err = missingDelete(f(ctx))
			if err == nil || !transient(err) || ctx.Err() != nil {
				break
			}
			if attempt+1 < cleanupAttempts {
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
