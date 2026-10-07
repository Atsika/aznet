package aznet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Listener implements net.Listener. Close owns sessions, not the shared
// bootstrap namespace; its administrator may explicitly call CleanupBootstrap.
type Listener struct {
	network    string
	ep         *Endpoint
	driver     Driver
	cfg        *Config
	conns      sync.Map
	acceptMu   sync.Mutex // owns pending and session publication
	pending    []Handshake
	closeOnce  sync.Once
	closeErr   error
	cleanupMu  sync.Mutex
	cleanupErr error // failures from rollback and janitor remain observable at Close
}

func (l *Listener) recordCleanup(err error) {
	if err == nil {
		return
	}
	l.cleanupMu.Lock()
	defer l.cleanupMu.Unlock()
	l.cleanupErr = errors.Join(l.cleanupErr, err)
}

func (l *Listener) Accept() (net.Conn, error) {
	l.acceptMu.Lock()
	defer l.acceptMu.Unlock()
	for {
		if l.cfg.ctx.Err() != nil {
			return nil, net.ErrClosed
		}
		if len(l.pending) == 0 {
			hs, err := l.driver.GetHandshakes(l.cfg.ctx)
			l.pending = append(l.pending, hs...)
			if l.cfg.ctx.Err() != nil {
				return nil, net.ErrClosed
			}
			if err != nil && !errors.Is(err, ErrNoData) {
				return nil, &AcceptError{"poll", err}
			}
			if len(l.pending) == 0 {
				timer := time.NewTimer(l.cfg.acceptPoll)
				select {
				case <-l.cfg.ctx.Done():
					timer.Stop()
					return nil, net.ErrClosed
				case <-timer.C:
				}
				continue
			}
		}
		hs := l.pending[0]
		l.pending[0] = Handshake{}
		l.pending = l.pending[1:]
		conn, err := l.acceptHandshake(hs)
		if conn != nil || err != nil {
			return conn, err
		}
	}
}

func (l *Listener) acceptHandshake(hs Handshake) (conn net.Conn, err error) {
	var owner *sessionOwner
	var tokens SessionTokens
	var transport Transport
	var noise *Noise
	handshakeAttempted := false
	var deleteErr error
	deleteHandshake := func() error {
		handshakeAttempted = true
		deleteErr = cleanup("delete handshake", func(ctx context.Context) error { return l.driver.DeleteHandshake(ctx, hs.ID) })
		return deleteErr
	}
	op := "handshake"
	defer func() {
		if !handshakeAttempted {
			deleteHandshake()
		}
		if deleteErr != nil {
			err = errors.Join(err, deleteErr)
		}
		if l.cfg.ctx.Err() != nil {
			err = errors.Join(err, net.ErrClosed)
		}
		if err != nil {
			var rollback error
			if transport != nil {
				rollback = disposeTransport(transport)
			}
			if owner != nil {
				rollback = errors.Join(rollback, owner.close())
			}
			l.recordCleanup(errors.Join(deleteErr, rollback))
			err = &AcceptError{op, errors.Join(err, rollback)}
			conn = nil
			return
		}
		if owner != nil {
			ctx, cancel := context.WithCancel(l.cfg.ctx)
			c := newConn(ctx, cancel, transport, l.cfg, noise, l.driver, owner.id)
			c.session = owner
			c.sessionExpiry = tokens.ExpiresAt
			l.conns.Store(owner.id, c)
			conn = c
		}
	}()
	noise, e := NewNoiseServer()
	if e != nil {
		return nil, e
	}
	payload, e := noise.ReadMessage(hs.Payload)
	if e != nil {
		return nil, fmt.Errorf("%w: %w", ErrHandshakeFailed, e)
	}
	id := string(payload)
	if _, e := uuid.Parse(id); e != nil {
		return nil, fmt.Errorf("%w: invalid session ID", ErrHandshakeFailed)
	}
	if _, exists := l.conns.Load(id); exists {
		return nil, nil
	}
	owner = &sessionOwner{driver: l.driver, id: id}
	op = "create session"
	tokens, e = l.driver.CreateSession(l.cfg.ctx, id)
	if e != nil {
		return nil, e
	}
	if e = l.cfg.ctx.Err(); e != nil {
		return nil, e
	}
	encoded, e := json.Marshal(tokens)
	if e != nil {
		return nil, e
	}
	msg, e := noise.WriteMessage(encoded)
	if e != nil {
		return nil, e
	}
	if !noise.IsComplete() {
		return nil, ErrHandshakeIncomplete
	}
	// Finish all server acquisitions before advertising the session to its peer.
	op = "create transport"
	transport, e = l.driver.NewTransport(l.cfg.ctx, id, tokens, false)
	if e != nil {
		return nil, e
	}
	if e = l.cfg.ctx.Err(); e != nil {
		return nil, e
	}
	op = "delete handshake"
	if e = deleteHandshake(); e != nil {
		return nil, e
	}
	if e = l.cfg.ctx.Err(); e != nil {
		return nil, e
	}
	op = "post token"
	if e = l.driver.PostToken(l.cfg.ctx, id, msg); e != nil {
		return nil, e
	}
	return nil, nil
}

// ConnectionString returns bootstrap credentials for this listener.
func (l *Listener) ConnectionString() (string, error) {
	h, t, err := l.driver.CreateBootstrapTokens()
	if err != nil {
		return "", err
	}
	return l.ep.BuildConnURL(l.cfg, h, t), nil
}

func (l *Listener) Close() error {
	l.closeOnce.Do(func() {
		l.cfg.cancel()
		done := make(chan error, 1)
		go func() {
			closeConnections := func() {
				var wg sync.WaitGroup
				l.conns.Range(func(key, value any) bool {
					wg.Add(1)
					go func() {
						defer wg.Done()
						l.recordCleanup(value.(*Conn).Close())
						l.conns.Delete(key)
					}()
					return true
				})
				wg.Wait()
			}
			// A stuck acquisition must not prevent cleanup of existing sessions.
			closeConnections()
			l.acceptMu.Lock()
			defer l.acceptMu.Unlock()
			l.pending = nil
			// Catch publication that raced cancellation and the first snapshot.
			closeConnections()
			l.cleanupMu.Lock()
			defer l.cleanupMu.Unlock()
			done <- l.cleanupErr
		}()
		// Setup rollback can perform handshake, transport, token and session cleanup.
		timer := time.NewTimer(5*cleanupTimeout + closeGracePeriod)
		defer timer.Stop()
		select {
		case l.closeErr = <-done:
		case <-timer.C:
			l.closeErr = fmt.Errorf("listener cleanup incomplete: %w", context.DeadlineExceeded)
		}
	})
	return l.closeErr
}

// CleanupBootstrap removes the shared discovery namespace. Call only after all
// users of that namespace have stopped. It is deliberately independent of Close.
func (l *Listener) CleanupBootstrap(ctx context.Context) error { return l.driver.CleanupBootstrap(ctx) }
func (l *Listener) Addr() net.Addr {
	return ServiceAddr{l.network, l.ep.ServiceURL(), l.cfg.handshakeEndpoint}
}
func (l *Listener) janitor() {
	ticker := time.NewTicker(l.cfg.idleTimeout / 2)
	defer ticker.Stop()
	for {
		select {
		case <-l.cfg.ctx.Done():
			return
		case <-ticker.C:
			l.conns.Range(func(key, value any) bool {
				c := value.(*Conn)
				if c.closed.Load() != 0 || time.Since(time.Unix(0, c.peerLastSeen.Load())) > l.cfg.idleTimeout {
					l.recordCleanup(c.Close())
					l.conns.Delete(key)
				}
				return true
			})
		}
	}
}

// Listen is analogous to net.Listen. It takes a network type (e.g. "azblob")
// and an address (e.g. "account.blob.core.windows.net").
//
// Listen fails with ErrResourceBeingDeleted when Azure is still deleting
// resources left by a previous listener of the same name. That is a transient
// condition, but waiting it out is the caller's decision, not the library's.
func Listen(network, address string, opts ...Option) (net.Listener, error) {
	driver, ep, cfg, err := initialize(network, address, opts)
	if err != nil {
		return nil, err
	}

	l := &Listener{
		network: network,
		ep:      ep,
		driver:  driver,
		cfg:     cfg,
	}

	go l.janitor()

	return l, nil
}

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
