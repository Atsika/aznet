package aznet

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Conn implements net.Conn.
type Conn struct {
	transport     Transport
	rotator       Rotator // nil if transport doesn't support rotation
	driver        Driver
	ctx           context.Context
	closeErr      error
	limitFailure  atomic.Pointer[bufferFailure]
	writeGate     chan struct{}
	id            string
	cancel        context.CancelFunc
	bufs          *Buffers
	cfg           *Config
	noise         *Noise
	poll          *AdaptivePoll
	wake          chan struct{} // buffered(1) nudge from flush() to wake an idle reader
	readGate      chan struct{}
	flushGate     chan struct{} // owns encryption and pending chunks; acquire before wmu
	readDeadline  ioDeadline
	writeDeadline ioDeadline
	pending       pendingChunk // sealed chunk awaiting write or rotation retry; guarded by flushGate
	chunkSeq      uint64       // next chunk sequence; guarded by flushGate

	lastActive   atomic.Int64
	peerLastSeen atomic.Int64
	lastNudge    atomic.Int64 // UnixNano of last reader nudge; rate-limits wakes

	session      *sessionOwner
	cleanupToken sync.Once
	closeOnce    sync.Once
	// wmu guards the write buffer (bufs.Write). Acquired briefly inside flush()
	// to drain the buffer, then released before the transport.WriteRaw call.
	wmu sync.Mutex
	// rmu protects read buffers from recycling. readGate serializes the entire
	// receive operation, including transport calls and polling.
	rmu sync.Mutex

	closed      atomic.Uint32
	closedRead  atomic.Uint32
	closedWrite atomic.Uint32
	mtu         int
	readRemain  int
}

// Buffers encapsulates the internal bytes.Buffer instances used by a connection.
type Buffers struct {
	Enc   []byte // Encryption scratch space
	Dec   []byte // Decryption scratch space
	Read  bytes.Buffer
	Write bytes.Buffer
	Noise bytes.Buffer
}

var buffersPool = sync.Pool{
	New: func() any {
		return &Buffers{
			Enc: make([]byte, 0, 64*1024),
			Dec: make([]byte, 0, 64*1024),
		}
	},
}

func newConn(ctx context.Context, cancel context.CancelFunc, t Transport, cfg *Config, noise *Noise, driver Driver, connID string) *Conn {
	now := time.Now()

	c := &Conn{
		ctx:       ctx,
		cancel:    cancel,
		poll:      NewAdaptivePoll(cfg.fastPoll, cfg.dataPoll),
		transport: t,
		driver:    driver,
		id:        connID,
		cfg:       cfg,
		noise:     noise,
		wake:      make(chan struct{}, 1),
		readGate:  make(chan struct{}, 1),
		flushGate: make(chan struct{}, 1),
		writeGate: make(chan struct{}, 1),
		bufs:      buffersPool.Get().(*Buffers),
		mtu:       min(t.MaxRawSize()-NoiseOverhead, cfg.limits().Retry-NoiseOverhead, cfg.limits().Write-FrameHeaderSize) - FrameHeaderSize,
	}
	if r, ok := t.(Rotator); ok {
		c.rotator = r
	}
	c.peerLastSeen.Store(now.UnixNano())
	c.lastActive.Store(now.UnixNano())

	if cfg.pingInterval > 0 {
		go c.keepAlive()
	}

	return c
}

// closeGracePeriod bounds the entire best-effort graceful close, including a
// backend that fails to honor cancellation. Such a backend retains ownership of
// its buffers until it returns; teardown must never recycle them prematurely.
const closeGracePeriod = 250 * time.Millisecond

// Close attempts accepted data followed by FIN within 250 ms, then cancels I/O.
// Session cleanup then has two independently bounded two-second operations.
// A timeout means delivery, transport teardown or resource cleanup is unconfirmed.
// On an accepted connection, Close deletes session storage: uploading bytes does
// not acknowledge peer consumption. Use CloseWrite and wait for application-level
// completion before Close when the peer must receive the final response.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(1)
		timer := time.NewTimer(closeGracePeriod)
		defer timer.Stop()
		done := make(chan error, 1)
		go func() {
			c.wmu.Lock()
			hadBuffered := c.bufs != nil && c.bufs.Write.Len() > 0
			if c.closedWrite.Swap(1) == 0 && c.bufs != nil {
				BuildFrame(&c.bufs.Write, Frame{Type: MsgTypeFin})
			}
			c.wmu.Unlock()
			ctx, finish := c.writeDeadline.operation(c.ctx)
			err := c.flushContext(ctx)
			if cause := context.Cause(ctx); cause != nil {
				err = cause
			}
			// Listener shutdown deliberately cancels idle connections. Report
			// interrupted delivery only when accepted bytes were still buffered.
			if !hadBuffered && errors.Is(err, context.Canceled) {
				err = nil
			}
			finish()
			c.cancel()
			err = errors.Join(err, c.transport.Close())
			c.recycleBuffers()
			done <- err
		}()
		select {
		case c.closeErr = <-done:
		case <-timer.C:
			c.closeErr = os.ErrDeadlineExceeded
		}
		c.cancel()
		c.readDeadline.set(time.Time{})
		c.writeDeadline.set(time.Time{})
		if c.session != nil {
			c.closeErr = errors.Join(c.closeErr, c.session.close())
		}
	})
	return c.closeErr
}

func (c *Conn) recycleBuffers() {
	// Uninterruptible only in the teardown worker: active owners must finish.
	c.flushGate <- struct{}{}
	defer func() { <-c.flushGate }()
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if c.bufs != nil {
		c.bufs.Read.Reset()
		c.bufs.Write.Reset()
		c.bufs.Noise.Reset()
		c.bufs.Enc = c.bufs.Enc[:0]
		c.bufs.Dec = c.bufs.Dec[:0]
		buffersPool.Put(c.bufs)
		c.bufs = nil
	}
	c.pending = pendingChunk{}
}

func (c *Conn) ioError(ctx context.Context, err error) error {
	if failure := c.limitFailure.Load(); failure != nil {
		return failure.err
	}
	if c.closed.Load() == 1 {
		return net.ErrClosed
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return err
}

func (c *Conn) LocalAddr() net.Addr { return c.transport.LocalAddr() }

func (c *Conn) RemoteAddr() net.Addr { return c.transport.RemoteAddr() }

func (c *Conn) SetDeadline(t time.Time) error {
	if c.closed.Load() == 1 {
		return net.ErrClosed
	}
	c.readDeadline.set(t)
	c.writeDeadline.set(t)
	return nil
}

func (c *Conn) SetReadDeadline(t time.Time) error {
	if c.closed.Load() == 1 {
		return net.ErrClosed
	}
	c.readDeadline.set(t)
	return nil
}

func (c *Conn) SetWriteDeadline(t time.Time) error {
	if c.closed.Load() == 1 {
		return net.ErrClosed
	}
	c.writeDeadline.set(t)
	return nil
}

// MTU returns the maximum number of application bytes that can fit in a single
// transport frame for the current connection.
func (c *Conn) MTU() int {
	return c.mtu
}

func (c *Conn) GetMetrics() Metrics { return c.cfg.metrics }
