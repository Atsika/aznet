package aznet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/google/uuid"
)

func TestCleanupPreservesJoinedFailures(t *testing.T) {
	missing := &azcore.ResponseError{StatusCode: 404}
	denied := &azcore.ResponseError{StatusCode: 403}
	for _, err := range []error{errors.Join(missing, denied), fmt.Errorf("custom driver: %w", errors.Join(missing, denied)), errors.Join(denied, missing)} {
		got := cleanup("session", func(context.Context) error { return err })
		if !errors.Is(got, denied) {
			t.Fatalf("hidden deletion failure: %v", got)
		}
	}
	if err := cleanup("already removed", func(context.Context) error { return fmt.Errorf("driver: %w", errors.Join(missing, missing)) }); err != nil {
		t.Fatal(err)
	}
}

type sessionDriver struct {
	Driver
	batch                                                    []Handshake
	poll                                                     func(context.Context) ([]Handshake, error)
	create                                                   func(context.Context, string) (SessionTokens, error)
	transport                                                func(context.Context) (Transport, error)
	post                                                     func(context.Context) error
	deletion                                                 func(context.Context) error
	sessionCleanup                                           func(context.Context) error
	polls, creates, tokens, sessions, handshakes, bootstraps atomic.Int32
}

func (d *sessionDriver) GetHandshakes(ctx context.Context) ([]Handshake, error) {
	d.polls.Add(1)
	if d.poll != nil {
		return d.poll(ctx)
	}
	h := d.batch
	d.batch = nil
	return h, nil
}

func (d *sessionDriver) CreateSession(ctx context.Context, id string) (SessionTokens, error) {
	d.creates.Add(1)
	if d.create != nil {
		return d.create(ctx, id)
	}
	return SessionTokens{}, nil
}

func (d *sessionDriver) NewTransport(ctx context.Context, _ string, _ SessionTokens, _ bool) (Transport, error) {
	if d.transport != nil {
		return d.transport(ctx)
	}
	return &reviewTransport{}, nil
}

func (d *sessionDriver) PostToken(ctx context.Context, _ string, _ []byte) error {
	if d.post != nil {
		return d.post(ctx)
	}
	return nil
}

func (d *sessionDriver) DeleteHandshake(ctx context.Context, _ string) error {
	d.handshakes.Add(1)
	if d.deletion != nil {
		return d.deletion(ctx)
	}
	return nil
}

func (d *sessionDriver) DeleteToken(context.Context, string) error {
	d.tokens.Add(1)
	return nil
}

func (d *sessionDriver) CleanupSession(ctx context.Context, _ string) error {
	d.sessions.Add(1)
	if d.sessionCleanup != nil {
		return d.sessionCleanup(ctx)
	}
	return nil
}

func (d *sessionDriver) CleanupBootstrap(context.Context) error {
	d.bootstraps.Add(1)
	return nil
}

func sessionListener(t *testing.T, d *sessionDriver, n int) *Listener {
	t.Helper()
	for i := 0; i < n; i++ {
		noise, _ := NewNoiseClient()
		id := uuid.NewString()
		msg, err := noise.WriteMessage([]byte(id))
		if err != nil {
			t.Fatal(err)
		}
		d.batch = append(d.batch, Handshake{id, msg})
	}
	cfg := applyConfig([]Option{WithPing(0), WithAcceptPoll(time.Millisecond)})
	l := &Listener{driver: d, cfg: cfg}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestAcceptRetainsDequeuedBatch(t *testing.T) {
	d := &sessionDriver{}
	l := sessionListener(t, d, 40)
	for i := 0; i < 40; i++ {
		if _, err := l.Accept(); err != nil {
			t.Fatal(err)
		}
	}
	if d.polls.Load() != 1 || d.creates.Load() != 40 {
		t.Fatalf("polls=%d creates=%d", d.polls.Load(), d.creates.Load())
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if d.sessions.Load() != 40 || d.tokens.Load() != 40 || d.bootstraps.Load() != 0 {
		t.Fatalf("session=%d token=%d bootstrap=%d", d.sessions.Load(), d.tokens.Load(), d.bootstraps.Load())
	}
	if err := l.CleanupBootstrap(context.Background()); err != nil || d.bootstraps.Load() != 1 {
		t.Fatal(err)
	}
}

func TestAcceptAcquisitionRollback(t *testing.T) {
	failure := errors.New("acquisition failure")
	for _, stage := range []string{"session", "transport", "token", "handshake"} {
		t.Run(stage, func(t *testing.T) {
			d := &sessionDriver{}
			l := sessionListener(t, d, 2)
			switch stage {
			case "session":
				d.create = func(context.Context, string) (SessionTokens, error) { return SessionTokens{}, failure }
			case "transport":
				d.transport = func(context.Context) (Transport, error) { return &reviewTransport{}, failure }
			case "token":
				d.post = func(context.Context) error { return failure }
			case "handshake":
				d.deletion = func(context.Context) error { return failure }
			}
			if c, err := l.Accept(); c != nil || !errors.Is(err, failure) {
				t.Fatalf("%v %v", c, err)
			}
			if d.sessions.Load() != 1 || d.tokens.Load() != 1 || d.handshakes.Load() != 1 {
				t.Fatalf("rollback: %d %d %d", d.sessions.Load(), d.tokens.Load(), d.handshakes.Load())
			}
			d.create = nil
			d.transport = nil
			d.post = nil
			d.deletion = nil
			if _, err := l.Accept(); err != nil {
				t.Fatal(err)
			}
			if d.polls.Load() != 1 {
				t.Fatal("lost batch remainder")
			}
		})
	}
}

func TestAcceptFailureClassification(t *testing.T) {
	for _, status := range []int{403, 404, 429, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			failure := &azcore.ResponseError{StatusCode: status, ErrorCode: "injected"}
			d := &sessionDriver{poll: func(context.Context) ([]Handshake, error) { return nil, failure }}
			l := sessionListener(t, d, 0)
			_, err := l.Accept()
			var ae *AcceptError
			if !errors.As(err, &ae) || !errors.Is(err, failure) || ae.Temporary() != (status == 429 || status == 503) || ae.Op != "poll" {
				t.Fatal(err)
			}
			if d.polls.Load() != 1 {
				t.Fatal("implicit retry")
			}
			d.poll = func(context.Context) ([]Handshake, error) {
				l.cfg.cancel()
				return nil, ErrNoData
			}
			if _, err = l.Accept(); !errors.Is(err, net.ErrClosed) {
				t.Fatal(err)
			}
		})
	}
}

func TestEmptyAcceptPollingCancellation(t *testing.T) {
	for _, emptyErr := range []error{nil, ErrNoData} {
		d := &sessionDriver{poll: func(context.Context) ([]Handshake, error) { return nil, emptyErr }}
		l := sessionListener(t, d, 0)
		l.cfg.acceptPoll = time.Hour
		done := make(chan error, 1)
		go func() {
			_, err := l.Accept()
			done <- err
		}()
		for d.polls.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		l.cfg.cancel()
		select {
		case err := <-done:
			if !errors.Is(err, net.ErrClosed) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("poll sleep not interrupted")
		}
	}
}

func TestShutdownDuringAcquisition(t *testing.T) {
	for _, stage := range []string{"create", "transport", "post"} {
		t.Run(stage, func(t *testing.T) {
			entered := make(chan struct{})
			block := func(ctx context.Context) error {
				close(entered)
				<-ctx.Done()
				return ctx.Err()
			}
			d := &sessionDriver{}
			l := sessionListener(t, d, 1)
			switch stage {
			case "create":
				d.create = func(ctx context.Context, _ string) (SessionTokens, error) { return SessionTokens{}, block(ctx) }
			case "transport":
				d.transport = func(ctx context.Context) (Transport, error) { return nil, block(ctx) }
			case "post":
				d.post = block
			}
			accepted := make(chan error, 1)
			go func() {
				_, err := l.Accept()
				accepted <- err
			}()
			<-entered
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					l.Close()
				}()
			}
			wg.Wait()
			if err := <-accepted; !errors.Is(err, net.ErrClosed) {
				t.Fatal(err)
			}
			if d.sessions.Load() != 1 || d.tokens.Load() != 1 || d.bootstraps.Load() != 0 {
				t.Fatal("cleanup ownership")
			}
		})
	}
}

func TestSessionCloseCleanupFailuresBoundedAndStable(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status, want int
	}{{"permanent", 403, 1}, {"transient", 503, 3}, {"absent", 404, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			failure := &azcore.ResponseError{StatusCode: tc.status}
			d := &sessionDriver{sessionCleanup: func(context.Context) error { return failure }}
			l := sessionListener(t, d, 1)
			c, err := l.Accept()
			if err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			errs := make(chan error, 12)
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					errs <- c.Close()
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if errors.Is(err, failure) != (tc.status != 404) {
					t.Fatal(err)
				}
			}
			if int(d.sessions.Load()) != tc.want {
				t.Fatalf("attempts=%d", d.sessions.Load())
			}
			if err := l.Close(); errors.Is(err, failure) != (tc.status != 404) {
				t.Fatal(err)
			}
		})
	}
}

func TestCleanupUncooperativeDriverBound(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	started := time.Now()
	err := cleanup("blocked", func(context.Context) error {
		calls.Add(1)
		<-release
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 3*time.Second || calls.Load() != 1 {
		t.Fatal(err, calls.Load(), time.Since(started))
	}
}

func TestTransientAcceptCanRecover(t *testing.T) {
	d := &sessionDriver{}
	l := sessionListener(t, d, 1)
	hs := d.batch
	d.batch = nil
	failure := &azcore.ResponseError{StatusCode: 503}
	d.poll = func(context.Context) ([]Handshake, error) { return nil, failure }
	if _, err := l.Accept(); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	d.poll = func(context.Context) ([]Handshake, error) { return hs, nil }
	if _, err := l.Accept(); err != nil {
		t.Fatal(err)
	}
}

func TestJanitorCleanupFailureSurvivesShutdown(t *testing.T) {
	failure := errors.New("cannot delete session")
	done := make(chan struct{}, 1)
	d := &sessionDriver{sessionCleanup: func(context.Context) error {
		done <- struct{}{}
		return failure
	}}
	l := sessionListener(t, d, 1)
	l.cfg.idleTimeout = 2 * time.Millisecond
	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	c.(*Conn).peerLastSeen.Store(0)
	go l.janitor()
	<-done
	if err := l.Close(); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if d.sessions.Load() != 1 {
		t.Fatal("duplicate cleanup")
	}
}

func TestCleanupTransientRecovery(t *testing.T) {
	var calls atomic.Int32
	err := cleanup("test", func(context.Context) error {
		if calls.Add(1) < 3 {
			return &azcore.ResponseError{StatusCode: 503}
		}
		return nil
	})
	if err != nil || calls.Load() != 3 {
		t.Fatal(err, calls.Load())
	}
}

func TestMalformedHandshakeDoesNotAllocate(t *testing.T) {
	d := &sessionDriver{}
	l := sessionListener(t, d, 1)
	d.batch = append([]Handshake{{ID: "bad", Payload: []byte("bad")}}, d.batch...)
	if _, err := l.Accept(); !errors.Is(err, ErrHandshakeFailed) {
		t.Fatal(err)
	}
	if d.creates.Load() != 0 || d.sessions.Load() != 0 {
		t.Fatal("allocated invalid session")
	}
	if _, err := l.Accept(); err != nil {
		t.Fatal(err)
	}
}

type dialFactory struct{ driver *dialDriver }

func (f dialFactory) NewDriver(_ *Endpoint, cfg *Config) (Driver, error) {
	f.driver.cfg = cfg
	return f.driver, nil
}

type dialDriver struct {
	*sessionDriver
	cfg     *Config
	token   []byte
	fail    string
	failure error
}

func (d *dialDriver) PostHandshake(ctx context.Context, _ string, msg []byte) error {
	if d.fail == "post" {
		return d.failure
	}
	if d.fail == "cancel" {
		<-ctx.Done()
		return ctx.Err()
	}
	noise, _ := NewNoiseServer()
	if _, err := noise.ReadMessage(msg); err != nil {
		return err
	}
	d.token, _ = noise.WriteMessage([]byte(`{"req":"r","res":"s"}`))
	return nil
}

func (d *dialDriver) GetToken(context.Context, string) ([]byte, error) {
	if d.fail == "token" {
		return nil, d.failure
	}
	return d.token, nil
}

type countedTransport struct {
	reviewTransport
	closes atomic.Int32
}

func (t *countedTransport) Close() error {
	t.closes.Add(1)
	return nil
}

func TestDialFailureOwnership(t *testing.T) {
	for _, stage := range []string{"post", "token", "transport", "cancel"} {
		t.Run(stage, func(t *testing.T) {
			failure := errors.New("dial failure")
			d := &dialDriver{sessionDriver: &sessionDriver{}, fail: stage, failure: failure}
			tr := &countedTransport{}
			d.transport = func(context.Context) (Transport, error) { return tr, failure }
			RegisterFactory("testdial", dialFactory{d})
			defer UnregisterFactory("testdial")
			c, err := Dial("testdial", "http://example.invalid", WithPing(0), WithConnectTimeout(5*time.Millisecond))
			if c != nil || err == nil {
				t.Fatal(c, err)
			}
			if stage == "cancel" {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			} else if !errors.Is(err, failure) {
				t.Fatal(err)
			}
			if d.cfg.ctx.Err() == nil {
				t.Fatal("dial context retained after failed setup")
			}
			if d.sessions.Load() != 0 || d.bootstraps.Load() != 0 {
				t.Fatal("initiator deleted server resources")
			}
			want := int32(0)
			if stage == "transport" {
				want = 1
			}
			if tr.closes.Load() != want {
				t.Fatal("partial transport not closed", tr.closes.Load())
			}
		})
	}
}

func TestCloseCleansActiveSessionsBeforeBlockedSetupReturns(t *testing.T) {
	d := &sessionDriver{}
	l := sessionListener(t, d, 2)
	if _, err := l.Accept(); err != nil {
		t.Fatal(err)
	}
	entered, release, cleaned := make(chan struct{}), make(chan struct{}), make(chan struct{}, 2)
	d.create = func(context.Context, string) (SessionTokens, error) {
		close(entered)
		<-release
		return SessionTokens{}, nil
	}
	d.sessionCleanup = func(context.Context) error {
		cleaned <- struct{}{}
		return nil
	}
	acceptDone := make(chan error, 1)
	go func() {
		_, err := l.Accept()
		acceptDone <- err
	}()
	<-entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- l.Close() }()
	select {
	case <-cleaned:
		close(release)
	case <-time.After(time.Second):
		close(release)
		t.Fatal("active cleanup waited for blocked acquisition")
	}
	<-closeDone
	if err := <-acceptDone; !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	if d.sessions.Load() != 2 {
		t.Fatal("late setup leaked", d.sessions.Load())
	}
}

func TestUnknownSessionExpirySerialization(t *testing.T) {
	raw, err := json.Marshal(SessionTokens{Req: "custom", Res: "opaque"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "expires_at") {
		t.Fatal("unknown metadata must be absent from handshake")
	}
	var legacy SessionTokens
	if err := json.Unmarshal([]byte(`{"req":"custom","res":"opaque"}`), &legacy); err != nil || !legacy.ExpiresAt.IsZero() {
		t.Fatalf("legacy tokens: %v %v", legacy, err)
	}
}
