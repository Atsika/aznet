package aznet

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Conn implements net.Conn.
type Conn struct {
	sessionExpiry time.Time // immutable issuance metadata; zero means unknown
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
	flushGate     chan struct{}         // owns encryption and pending chunks; acquire before wmu
	flushKick     chan struct{}         // buffered(1); wakes the batching flusher, nil when synchronous
	writeSpace    chan struct{}         // buffered(1); flushLoop freed bytes or failed
	flushErr      atomic.Pointer[error] // flushLoop failure owed to the next Write
	readDeadline  ioDeadline
	writeDeadline ioDeadline
	pending       pendingChunk // sealed chunk awaiting write or rotation retry; guarded by flushGate
	chunkSeq      uint64       // next chunk sequence; guarded by flushGate
	// tailFrame is the offset in bufs.Write of the last frame if it is DATA and
	// may still grow, else -1; sealedLen is the leading bytes already sealed
	// into a chunk, which must never change. Both guarded by wmu.
	tailFrame int
	sealedLen int

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
		ctx:        ctx,
		cancel:     cancel,
		poll:       NewAdaptivePoll(cfg.fastPoll, cfg.dataPoll),
		transport:  t,
		driver:     driver,
		id:         connID,
		cfg:        cfg,
		noise:      noise,
		wake:       make(chan struct{}, 1),
		readGate:   make(chan struct{}, 1),
		flushGate:  make(chan struct{}, 1),
		writeGate:  make(chan struct{}, 1),
		flushKick:  make(chan struct{}, 1),
		writeSpace: make(chan struct{}, 1),
		bufs:       buffersPool.Get().(*Buffers),
		tailFrame:  -1,
		mtu:        min(t.MaxRawSize()-NoiseOverhead, cfg.limits().Retry-NoiseOverhead, cfg.limits().Write-FrameHeaderSize) - FrameHeaderSize,
	}
	if r, ok := t.(Rotator); ok {
		c.rotator = r
	}
	c.peerLastSeen.Store(now.UnixNano())
	c.lastActive.Store(now.UnixNano())

	if cfg.pingInterval > 0 {
		go c.keepAlive()
	}
	go c.flushLoop()

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
				c.appendControl(MsgTypeFin)
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
		c.tailFrame, c.sealedLen = -1, 0
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

// ioDeadline cancels all current operations when its mutable deadline expires.
// Moving or clearing a deadline before expiry leaves those operations alive.
type ioDeadline struct {
	active     map[context.Context]context.CancelCauseFunc
	timer      *time.Timer
	when       time.Time
	mu         sync.Mutex
	generation uint64
}

// expired checks the deadline for a nonblocking operation. Blocking operations
// must instead register with operation so subsequent deadline changes cancel I/O.
func (d *ioDeadline) expired() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.when.IsZero() && !time.Now().Before(d.when)
}

func (d *ioDeadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.when = t
	d.generation++
	generation := d.generation
	if d.timer != nil {
		d.timer.Stop()
	}
	if t.IsZero() {
		return
	}
	expire := func() {
		for _, cancel := range d.active {
			cancel(os.ErrDeadlineExceeded)
		}
	}
	if !time.Now().Before(t) {
		expire()
		return
	}
	d.timer = time.AfterFunc(time.Until(t), func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.generation == generation {
			expire()
		}
	})
}

func (d *ioDeadline) operation(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	d.mu.Lock()
	if d.active == nil {
		d.active = make(map[context.Context]context.CancelCauseFunc)
	}
	d.active[ctx] = cancel
	if !d.when.IsZero() && !time.Now().Before(d.when) {
		cancel(os.ErrDeadlineExceeded)
	}
	d.mu.Unlock()
	return ctx, func() {
		d.mu.Lock()
		delete(d.active, ctx)
		d.mu.Unlock()
		cancel(context.Canceled)
	}
}

// lockIO makes waiting for another operation interruptible as well as the I/O.
func lockIO(ctx context.Context, gate chan struct{}) error {
	select {
	case gate <- struct{}{}:
		if err := context.Cause(ctx); err != nil {
			<-gate
			return err
		}
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

type bufferFailure struct{ err error }

func (c *Conn) overflow(stage string) error {
	c.limitFailure.CompareAndSwap(nil, &bufferFailure{err: fmt.Errorf("%w: %s", ErrBufferLimit, stage)})
	c.cancel()
	return c.limitFailure.Load().err
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

func (c *Conn) Read(p []byte) (n int, err error) {
	if n, ok := c.readBuffered(p); ok {
		return n, nil
	}
	ctx, finish := c.readDeadline.operation(c.ctx)
	defer finish()
	defer func() {
		if err != nil {
			err = c.ioError(ctx, err)
		}
	}()
	if err := lockIO(ctx, c.readGate); err != nil {
		return 0, err
	}
	defer func() { <-c.readGate }()
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if c.closed.Load() == 1 {
			return 0, net.ErrClosed
		}

		c.rmu.Lock()
		if c.closedRead.Load() == 1 {
			c.rmu.Unlock()
			return 0, io.EOF
		}
		// Close() recycles bufs into the shared pool while holding rmu, so a
		// non-nil check under the lock is what keeps this read off a buffer that
		// now belongs to another connection.
		if c.bufs == nil {
			c.rmu.Unlock()
			return 0, net.ErrClosed
		}

		if err := context.Cause(ctx); err != nil {
			c.rmu.Unlock()
			return 0, err
		}

		// Drain leftover payload from a previous partial read.
		if c.readRemain > 0 {
			n := copy(p, c.bufs.Read.Next(min(c.readRemain, len(p))))
			c.readRemain -= n
			c.rmu.Unlock()
			return n, nil
		}

		// Peek at next frame header without consuming payload.
		if c.bufs.Read.Len() >= FrameHeaderSize {
			header := c.bufs.Read.Bytes()[:FrameHeaderSize]
			fType := header[4]
			fLen64 := uint64(binary.BigEndian.Uint32(header[:4]))
			if fLen64+FrameHeaderSize > uint64(c.cfg.limits().Decrypted) {
				c.rmu.Unlock()
				return 0, c.overflow("decrypted frame")
			}
			fLen := int(fLen64)

			if c.bufs.Read.Len() >= FrameHeaderSize+fLen {
				c.peerLastSeen.Store(time.Now().UnixNano())
				switch fType {
				case MsgTypeData:
					// Consume header, then read min(fLen, len(p)) from payload.
					c.bufs.Read.Next(FrameHeaderSize)
					n := copy(p, c.bufs.Read.Next(min(fLen, len(p))))
					c.readRemain = fLen - n
					c.rmu.Unlock()
					return n, nil
				case MsgTypePing:
					c.bufs.Read.Next(FrameHeaderSize + fLen)
					c.rmu.Unlock()
					continue
				case MsgTypeFin:
					c.bufs.Read.Next(FrameHeaderSize + fLen)
					c.closedRead.Store(1)
					c.rmu.Unlock()
					return 0, io.EOF
				case MsgTypeRotate:
					c.bufs.Read.Next(FrameHeaderSize + fLen)
					if c.rotator != nil {
						_ = c.rotator.RotateRX()
					}
					c.rmu.Unlock()
					continue
				default:
					c.bufs.Read.Next(FrameHeaderSize + fLen)
					c.rmu.Unlock()
					continue
				}
			}
		}

		// A previous operation may have received ciphertext before its deadline
		// interrupted decryption. Process that owned data before polling again.
		maxChunk := min(c.transport.MaxRawSize(), c.cfg.limits().Pending)
		if c.bufs.Noise.Len() >= 4 {
			sealedSize := uint64(binary.BigEndian.Uint32(c.bufs.Noise.Bytes()[:4])) + 4
			available := c.cfg.limits().Decrypted - c.bufs.Read.Len()
			if sealedSize > uint64(available)+NoiseOverhead {
				c.rmu.Unlock()
				return 0, c.overflow("decrypted chunk")
			}
		}
		decrypted, rest, decodeErr := c.noise.UnsealData(c.bufs.Dec, c.bufs.Noise.Bytes(), maxChunk)
		if decodeErr != nil && decodeErr != io.ErrShortBuffer {
			c.rmu.Unlock()
			if errors.Is(decodeErr, ErrChunkTooLarge) {
				return 0, c.overflow("ciphertext chunk")
			}
			return 0, decodeErr
		}
		if decodeErr == nil {
			if c.bufs.Read.Len() == 0 {
				// Transfer ownership instead of copying every decrypted chunk.
				// The old, fully consumed buffer becomes the next scratch space;
				// decryption must never reuse the plaintext still owned by Read.
				c.bufs.Read.Reset()
				spare := c.bufs.Read.AvailableBuffer()
				c.bufs.Read = *bytes.NewBuffer(decrypted)
				c.bufs.Dec = spare
			} else {
				// A peer may split a frame across encrypted chunks. Preserve the
				// incomplete prefix and append the newly authenticated suffix.
				c.bufs.Dec = decrypted[:0]
				c.bufs.Read.Write(decrypted)
			}
			c.cleanupToken.Do(func() {
				if c.session != nil {
					go c.session.deleteToken()
				}
			})
			used := c.bufs.Noise.Len() - len(rest)
			c.bufs.Noise.Next(used)
			c.rmu.Unlock()
			// Drain framed plaintext before decrypting the next chunk.
			continue
		}
		receiveBudget := c.cfg.limits().Pending - c.bufs.Noise.Len()
		c.rmu.Unlock()

		// Fetch more data
		var rawStream io.ReadCloser
		var err error
		if reader, ok := c.transport.(limitedRawReader); ok {
			rawStream, err = reader.ReadRawLimit(ctx, receiveBudget)
		} else {
			rawStream, err = c.transport.ReadRaw(ctx)
		}
		if err != nil {
			if errors.Is(err, ErrBufferLimit) {
				return 0, c.overflow("transport receive")
			}
			if errors.Is(err, ErrNoData) {
				if err := c.idleWait(ctx); err != nil {
					return 0, err
				}
				continue
			}
			if errors.Is(err, context.Canceled) && c.closed.Load() == 1 {
				return 0, net.ErrClosed
			}
			return 0, err
		}

		// Read directly from the stream into the Noise buffer, then decrypt.
		// readGate covers fetching, body consumption, decryption and frame parsing.
		// rmu additionally protects buffer recycling during teardown.
		c.rmu.Lock()
		if c.bufs == nil {
			c.rmu.Unlock()
			rawStream.Close()
			return 0, net.ErrClosed
		}

		// Closing the response body interrupts a blocked stream read as well
		// as canceling the context passed to ReadRaw.
		bodyClosed := make(chan struct{})
		stopClose := context.AfterFunc(ctx, func() {
			rawStream.Close()
			close(bodyClosed)
		})
		remaining := c.cfg.limits().Pending - c.bufs.Noise.Len()
		_, err = c.bufs.Noise.ReadFrom(io.LimitReader(rawStream, int64(remaining)))
		if err == nil {
			var extra [1]byte
			n, probeErr := io.ReadFull(rawStream, extra[:])
			if n > 0 {
				err = c.overflow("pending ciphertext")
			} else if probeErr != io.EOF {
				err = probeErr
			}
		}
		if stopClose() {
			rawStream.Close()
		} else {
			<-bodyClosed
		}
		if cause := context.Cause(ctx); cause != nil {
			err = cause
		}
		if err != nil && err != io.EOF {
			c.rmu.Unlock()
			return 0, err
		}

		c.rmu.Unlock()
		c.poll.Reset()
	}
}

// readBuffered handles the remainder of an already authenticated data frame.
// No transport call or wait is needed, so avoid allocating an operation context
// and registering it with the deadline manager for each small application read.
// Contention and every exceptional state use the ordinary interruptible path.
func (c *Conn) readBuffered(p []byte) (int, bool) {
	if len(p) == 0 {
		return 0, false
	}
	select {
	case c.readGate <- struct{}{}:
	default:
		return 0, false
	}
	defer func() { <-c.readGate }()
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if c.closed.Load() != 0 || c.closedRead.Load() != 0 || c.bufs == nil || c.readRemain == 0 {
		return 0, false
	}
	if context.Cause(c.ctx) != nil || c.readDeadline.expired() {
		return 0, false
	}
	n := copy(p, c.bufs.Read.Next(min(c.readRemain, len(p))))
	c.readRemain -= n
	return n, true
}

// nudgeReader hints the read loop that a reply is likely imminent (we just sent),
// collapsing its poll back-off to fetch sooner. Rate-limited to one nudge per
// dataPoll so a one-directional writer cannot pin the idle reverse channel at
// fast-poll rate and bill extra reads.
func (c *Conn) nudgeReader() {
	now := time.Now().UnixNano()
	last := c.lastNudge.Load()
	if now-last < int64(c.cfg.dataPoll) {
		return
	}
	if !c.lastNudge.CompareAndSwap(last, now) {
		return
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// idleWait shares the operation context so deadline updates interrupt polling.
func (c *Conn) idleWait(ctx context.Context) error {
	timer := time.NewTimer(c.poll.Next())
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-c.wake:
		c.poll.Reset()
		return nil
	case <-timer.C:
		return nil
	}
}

// pendingChunk holds a sealed chunk whose write failed, for verbatim resend.
// Re-sealing is not an option: Noise nonces advance per seal, so a second seal
// of the same plaintext would desync the peer permanently. Guarded by flushGate.
type pendingChunk struct {
	data    []byte // sealed ciphertext, owned copy
	seq     uint64 // chunk sequence, reused so the retry is idempotent
	consume int    // bytes of bufs.Write this chunk covers (0 for control chunks)
	rotate  bool   // call RotateTX once the write lands
	valid   bool   // data holds a chunk still owed to the peer
	sent    bool   // ciphertext landed, but rotation may still need committing
}

// Write queues p and returns once it is accepted; flushLoop sends it in order.
// Bytes queued while a chunk is in flight leave together in the next chunk, so
// small writes share storage requests without a coalescing timer. Write blocks
// only while BufferLimits.Write is full. n reports bytes the connection owns,
// including on error; callers must only resubmit p[n:]. A failed background
// send is returned by the next Write (n == 0) and its retained ciphertext is
// resent on the following flush. Write(nil) and CloseWrite wait for everything
// queued to be sent.
func (c *Conn) Write(p []byte) (int, error) {
	if c.closed.Load() == 1 || c.closedWrite.Load() == 1 {
		return 0, io.ErrClosedPipe
	}
	ctx, finish := c.writeDeadline.operation(c.ctx)
	defer finish()
	if err := context.Cause(ctx); err != nil {
		return 0, c.ioError(ctx, err)
	}

	if err := lockIO(ctx, c.writeGate); err != nil {
		return 0, c.ioError(ctx, err)
	}
	defer func() { <-c.writeGate }()
	if len(p) == 0 {
		// A barrier that reports its own outcome, so it is not owed again.
		err := c.flushContext(ctx)
		c.flushErr.Store(nil)
		return 0, c.ioError(ctx, err)
	}
	accepted := 0
	for len(p) > 0 {
		if failed := c.flushErr.Swap(nil); failed != nil {
			return accepted, c.ioError(ctx, *failed)
		}
		c.wmu.Lock()
		if c.bufs == nil || c.closed.Load() == 1 || c.closedWrite.Load() == 1 {
			c.wmu.Unlock()
			return accepted, io.ErrClosedPipe
		}
		if c.mtu <= 0 {
			c.wmu.Unlock()
			return accepted, c.overflow("transport capacity")
		}
		// Reserve a FIN header so shutdown never needs to exceed the allowance.
		available := c.cfg.limits().Write - FrameHeaderSize - c.bufs.Write.Len()
		// Grow the last DATA frame while it is unsealed, so consecutive small
		// writes share one frame and fill chunks; a chunk still ends on a frame.
		if n := c.extendTail(p, available); n > 0 {
			p = p[n:]
			accepted += n
			available -= n
		}
		for len(p) > 0 && available > FrameHeaderSize {
			size := min(len(p), c.mtu)
			if size+FrameHeaderSize > available {
				break
			}
			c.tailFrame = c.bufs.Write.Len()
			BuildFrame(&c.bufs.Write, Frame{Type: MsgTypeData, Payload: p[:size]})
			p = p[size:]
			accepted += size
			available -= size + FrameHeaderSize
		}
		c.wmu.Unlock()
		select {
		case c.flushKick <- struct{}{}:
		default:
		}
		if len(p) == 0 {
			break
		}
		// Full: wait for flushLoop to free bytes or report a failure.
		select {
		case <-c.writeSpace:
		case <-ctx.Done():
			return accepted, c.ioError(ctx, context.Cause(ctx))
		}
	}
	return accepted, nil
}

// flushLoop sends batched writes. Each pass drains everything queued, so bytes
// that arrive while a chunk is in flight leave together in the next chunk.
func (c *Conn) flushLoop() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.flushKick:
		}
		_ = c.flushContext(c.ctx) // a failure is owed to the next Write
		c.signalWriteSpace()
	}
}

func (c *Conn) signalWriteSpace() {
	select {
	case c.writeSpace <- struct{}{}:
	default:
	}
}

// extendTail appends up to available bytes of p to the last frame when it is
// an unsealed DATA frame below the MTU, and returns how many it took. The
// frame's length header is rewritten in place. Caller holds wmu.
func (c *Conn) extendTail(p []byte, available int) int {
	if c.tailFrame < c.sealedLen {
		return 0
	}
	length := int(binary.BigEndian.Uint32(c.bufs.Write.Bytes()[c.tailFrame:]))
	n := min(len(p), c.mtu-length, available)
	if n <= 0 {
		return 0
	}
	c.bufs.Write.Write(p[:n])
	binary.BigEndian.PutUint32(c.bufs.Write.Bytes()[c.tailFrame:], uint32(length+n))
	return n
}

// appendControl queues a control frame, which data never merges across.
// Caller holds wmu.
func (c *Conn) appendControl(t byte) {
	BuildFrame(&c.bufs.Write, Frame{Type: t})
	c.tailFrame = -1
}

// CloseWrite shuts down the writing side of the connection. It sends a FIN frame
// to the peer to indicate that no more data will be sent.
func (c *Conn) CloseWrite() error {
	if c.closed.Load() == 1 || c.closedWrite.Swap(1) == 1 {
		return nil
	}
	c.wmu.Lock()
	if c.bufs == nil {
		c.wmu.Unlock()
		return net.ErrClosed
	}
	c.appendControl(MsgTypeFin)
	c.wmu.Unlock()

	err := c.flush()
	c.flushErr.Store(nil) // reported here
	return err
}

// keepAlive sends a Ping frame whenever nothing has been flushed for a full
// pingInterval.
func (c *Conn) keepAlive() {
	ticker := time.NewTicker(c.cfg.pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if c.closed.Load() == 1 || c.closedWrite.Load() == 1 {
				return
			}
			last := c.lastActive.Load()
			if time.Since(time.Unix(0, last)) >= c.cfg.pingInterval {
				c.wmu.Lock()
				if c.bufs == nil || c.closed.Load() == 1 || c.closedWrite.Load() == 1 {
					c.wmu.Unlock()
					return
				}
				if c.bufs.Write.Len()+2*FrameHeaderSize <= c.cfg.limits().Write {
					c.appendControl(MsgTypePing)
				}
				c.wmu.Unlock()
				_ = c.flush()
				continue
			}
		}
	}
}

func (c *Conn) flush() error {
	ctx, finish := c.writeDeadline.operation(c.ctx)
	defer finish()
	err := c.flushContext(ctx)
	if err != nil {
		return c.ioError(ctx, err)
	}
	return nil
}

// flushContext sends everything queued. Under flushGate it records the outcome
// for the next Write: a failure stays owed until a later flush succeeds, which
// also delivers the failed chunk, so a recovered failure is never reported.
func (c *Conn) flushContext(ctx context.Context) (err error) {
	if err := lockIO(ctx, c.flushGate); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			c.flushErr.Store(&err)
		} else {
			c.flushErr.Store(nil)
		}
		<-c.flushGate
	}()

	if c.bufs == nil {
		return net.ErrClosed
	}

	// Resend a failed chunk before sealing anything new, so chunks stay in nonce
	// order. Its original seq keeps the resend idempotent at the driver.
	if c.pending.valid {
		if err := c.sendChunk(ctx, c.pending.data, c.pending.consume, c.pending.rotate, c.pending.seq); err != nil {
			return err
		}
	}

	// Derived from mtu so the largest frame always fits in one chunk.
	maxChunk := int(c.mtu) + FrameHeaderSize

	for {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		c.wmu.Lock()
		if c.bufs.Write.Len() == 0 {
			c.wmu.Unlock()
			return nil
		}

		// Always at a frame boundary, since chunking below is frame-aligned.
		if c.rotator != nil && c.rotator.ShouldRotate() {
			c.wmu.Unlock()

			// Send rotation frame
			var rBuf bytes.Buffer
			BuildFrame(&rBuf, Frame{Type: MsgTypeRotate})

			sealed, err := c.noise.SealData(c.bufs.Enc, rBuf.Bytes())
			if err != nil {
				return err
			}
			c.bufs.Enc = sealed[:0]

			if err := c.sendChunk(ctx, sealed, 0, true, c.chunkSeq); err != nil {
				return err
			}
			continue // Re-check buffer after rotation
		}

		takeLen := alignedChunkLen(c.bufs.Write.Bytes(), maxChunk)
		if takeLen == 0 {
			// Framing is already broken; an unaligned chunk would hide it.
			c.wmu.Unlock()
			return fmt.Errorf("%w: frame exceeds chunk size %d", ErrFrameTooLarge, maxChunk)
		}

		// Seal while still holding wmu: the slice aliases the write buffer's
		// backing array, which a concurrent Write can slide in place.
		sealed, err := c.noise.SealData(c.bufs.Enc, c.bufs.Write.Bytes()[:takeLen])
		if err != nil {
			c.wmu.Unlock()
			return err
		}
		c.bufs.Enc = sealed[:0]
		c.sealedLen = takeLen
		c.wmu.Unlock()

		if err := c.sendChunk(ctx, sealed, takeLen, false, c.chunkSeq); err != nil {
			return err
		}

		c.lastActive.Store(time.Now().UnixNano())
		c.nudgeReader()
	}
}

// sendChunk writes one sealed chunk, consuming the plaintext it covered and
// applying any rotation only once the write lands; a failure leaves both queued
// for retry. Caller must hold flushGate and must not hold wmu.
func (c *Conn) sendChunk(ctx context.Context, sealed []byte, consume int, rotate bool, seq uint64) (err error) {
	if seq == ^uint64(0) {
		return c.overflow("sequence exhausted")
	}
	if len(sealed) > c.cfg.limits().Retry {
		return c.overflow("retry ciphertext")
	}
	sent := c.pending.valid && c.pending.sent
	defer func() {
		if err != nil {
			if !c.pending.valid {
				// Copy only on failure; the next encryption reuses bufs.Enc.
				c.pending = pendingChunk{data: append(c.pending.data[:0], sealed...), seq: seq, consume: consume, rotate: rotate, valid: true}
			}
			c.pending.sent = sent
		}
	}()
	if !sent {
		if err = c.transport.WriteRaw(ctx, seq, bytes.NewReader(sealed)); err != nil {
			if errors.Is(err, ErrBufferLimit) {
				return c.overflow("transport write")
			}
			return err
		}
		sent = true
	}
	if rotate {
		if err = c.rotator.RotateTX(ctx); err != nil {
			return err
		}
	}

	c.pending = pendingChunk{}
	c.chunkSeq = seq + 1

	if consume > 0 {
		c.wmu.Lock()
		c.bufs.Write.Next(consume)
		c.sealedLen = 0
		if c.tailFrame -= consume; c.tailFrame < 0 {
			c.tailFrame = -1
		}
		c.wmu.Unlock()
		c.signalWriteSpace()
	}
	return nil
}
