package aznet

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const (
	// MsgTypeData is for application data.
	MsgTypeData byte = 0x00
	// MsgTypePing is for keep-alive heartbeats.
	MsgTypePing byte = 0x01
	// MsgTypeFin is for graceful close.
	MsgTypeFin byte = 0x02
	// MsgTypeRotate is for rotation notifications.
	MsgTypeRotate byte = 0x03
)

// ErrBufferLimit is a terminal connection failure: retaining more bytes would
// exceed a configured allowance. Close still owns transport/session cleanup.
var ErrBufferLimit = errors.New("aznet: byte buffer limit exceeded")

// Handshake represents a discovered connection request.
type Handshake struct {
	ID      string // handshake identifier (used for cleanup)
	Payload []byte // The raw Noise handshake message
}

// SessionTokens represents the session-specific tokens/SAS exchanged after handshake.
type SessionTokens struct {
	Req string `json:"req"`
	Res string `json:"res"`
}

// Transport is the raw byte-exchange interface implemented by drivers.
type Transport interface {
	// WriteRaw sends raw bytes to the peer. seq is the chunk number, reused on a
	// retry so the driver can keep the write idempotent. Conn serializes calls,
	// starts at zero, and advances only after success; retries use identical bytes.
	// It must honor ctx.
	WriteRaw(ctx context.Context, seq uint64, data io.ReadSeeker) error
	// ReadRaw attempts to read raw bytes from the peer. The request and returned
	// stream must honor ctx; closing the stream must unblock a pending Read.
	ReadRaw(ctx context.Context) (io.ReadCloser, error)
	// Close terminates the transport.
	Close() error
	// LocalAddr returns the local network address.
	LocalAddr() net.Addr
	// RemoteAddr returns the remote network address.
	RemoteAddr() net.Addr
	// MaxRawSize returns the maximum raw capacity of the transport in bytes.
	// It must be constant for the lifetime of the transport.
	MaxRawSize() int
}

// limitedRawReader is an optional response-budget capability. Drivers such as
// Blob can stop a response mid-chunk and resume at the consumed byte offset.
// The base Transport interface remains sufficient; oversized base responses
// are rejected by the core's finite receive allowance.
type limitedRawReader interface {
	ReadRawLimit(context.Context, int) (io.ReadCloser, error)
}

// Rotator is optionally implemented by transports that need resource rotation
// (e.g., blob append blobs have a 50,000 block limit). Core handles rotation
// signaling automatically when this interface is satisfied.
type Rotator interface {
	ShouldRotate() bool
	RotateTX(ctx context.Context) error
	RotateRX() error
}

// ServiceAddr is a reusable net.Addr implementation for all drivers.
type ServiceAddr struct {
	Net      string // driver name (e.g. "azblob")
	Endpoint string // base service URL
	Resource string // resource identifier (container/queue/table + sub-resource)
}

func (a ServiceAddr) Network() string { return a.Net }
func (a ServiceAddr) String() string  { return a.Endpoint + "/" + a.Resource }

// Driver defines how a driver handles the initial connection setup.
type Driver interface {
	// Handshake operations
	PostHandshake(ctx context.Context, connID string, data []byte) error
	GetHandshakes(ctx context.Context) ([]Handshake, error)
	DeleteHandshake(ctx context.Context, id string) error

	// Token exchange operations
	PostToken(ctx context.Context, connID string, data []byte) error
	GetToken(ctx context.Context, connID string) ([]byte, error)
	DeleteToken(ctx context.Context, connID string) error

	// Session lifecycle. The listener owns connID before this call and invokes
	// CleanupSession even when acquisition fails partway through.
	CreateSession(ctx context.Context, connID string) (SessionTokens, error)
	CreateBootstrapTokens() (hSAS, tSAS string, err error)

	// NewTransport creates a data transporter for an established session.
	NewTransport(ctx context.Context, connID string, tokens SessionTokens, isInitiator bool) (Transport, error)

	// CleanupBootstrap removes shared bootstrap resources (handshake/token endpoints).
	CleanupBootstrap(ctx context.Context) error
	// CleanupSession removes all per-connection resources, including partial
	// acquisitions. It must be idempotent and return deletion failures.
	CleanupSession(ctx context.Context, connID string) error
}

// Factory is an interface for creating a Driver implementation.
type Factory interface {
	// NewDriver creates a Driver for the given endpoint and config.
	NewDriver(ep *Endpoint, cfg *Config) (Driver, error)
}

var factories = make(map[string]Factory)

var (
	// ErrUnsupportedScheme is returned when no registered driver exists for the requested URL scheme.
	ErrUnsupportedScheme = errors.New("unsupported scheme")
	// ErrClientCreationFailed is returned when an Azure service client cannot be created.
	ErrClientCreationFailed = errors.New("client creation failed")
	// ErrSASGenerationFailed is returned when a SAS token cannot be generated.
	ErrSASGenerationFailed = errors.New("failed to generate SAS token")
	// ErrMissingSAS is returned when required SAS tokens are missing from the URL.
	ErrMissingSAS = errors.New("missing handshake or token SAS in URL")
	// ErrInvalidSASEncoding is returned when a SAS token is not properly URL-encoded.
	ErrInvalidSASEncoding = errors.New("invalid SAS encoding")
	// ErrDecodeTokenFailed is returned when the JSON token payload cannot be decoded.
	ErrDecodeTokenFailed = errors.New("failed to decode token payload")
	// ErrWriteBufferFailed is returned when data cannot be written to an internal buffer.
	ErrWriteBufferFailed = errors.New("failed to write data to buffer")
	// ErrHandshakeExchangeFailed is returned when the initial handshake message cannot be sent or received.
	ErrHandshakeExchangeFailed = errors.New("failed to exchange handshake")
	// ErrInvalidConfig is returned when the provided options result in an invalid configuration.
	ErrInvalidConfig = errors.New("invalid configuration")
	// ErrNoData is returned when no data is available to read.
	ErrNoData = errors.New("no data available")
	// ErrFrameTooLarge is returned when a queued frame exceeds one chunk.
	ErrFrameTooLarge = errors.New("frame exceeds chunk size")
	// ErrResourceBeingDeleted is returned when a bootstrap resource cannot be
	// created because Azure is still deleting one of the same name, which it does
	// for roughly 30-40s after a listener stops. Retrying later succeeds; the
	// library reports the condition and leaves that decision to the caller.
	ErrResourceBeingDeleted = errors.New("storage resource is being deleted, retry once Azure has released the name")
)

// RegisterFactory registers a factory for the given scheme (e.g., "azblob").
func RegisterFactory(scheme string, factory Factory) {
	if _, dup := factories[scheme]; dup {
		panic("aznet: factory already registered for scheme " + scheme)
	}
	factories[scheme] = factory
}

// UnregisterFactory removes the factory registration.
func UnregisterFactory(scheme string) {
	delete(factories, scheme)
}

// GetFactories returns a list of registered factory names.
func GetFactories() []string {
	schemes := make([]string, 0, len(factories))
	for scheme := range factories {
		schemes = append(schemes, scheme)
	}
	sort.Strings(schemes)
	return schemes
}

func lookupFactory(scheme string) (Factory, bool) {
	factory, ok := factories[scheme]
	return factory, ok
}

func initialize(network, address string, opts []Option) (Driver, *Endpoint, *Config, error) {
	factory, ok := lookupFactory(network)
	if !ok {
		return nil, nil, nil, fmt.Errorf("%w: %s", ErrUnsupportedScheme, network)
	}

	cfg := applyConfig(opts)
	if err := cfg.Validate(); err != nil {
		cfg.cancel()
		return nil, nil, nil, err
	}

	u, err := url.Parse(address)
	if err != nil {
		cfg.cancel()
		return nil, nil, nil, err
	}
	ep := NewEndpoint(u)

	driver, err := factory.NewDriver(ep, cfg)
	if err != nil {
		cfg.cancel()
		return nil, nil, nil, err
	}

	return &metricsDriver{Driver: driver, m: cfg.metrics}, ep, cfg, nil
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

// Dial is analogous to net.Dial. It takes a network type (e.g. "azblob")
// and an address (e.g. "https://account.blob.core.windows.net/?handshake=...").
func Dial(network, address string, opts ...Option) (conn net.Conn, err error) {
	driver, _, cfg, err := initialize(network, address, opts)
	if err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			cfg.cancel()
		}
	}()
	dialCtx, dialCancel := context.WithTimeout(cfg.ctx, cfg.connectTimeout)
	defer dialCancel()

	connID := uuid.New().String()
	noise, err := NewNoiseClient()
	if err != nil {
		return nil, err
	}
	msg1, err := noise.WriteMessage([]byte(connID))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoiseMsgFailed, err)
	}

	if err := driver.PostHandshake(dialCtx, connID, msg1); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrHandshakeExchangeFailed, err)
	}

	var encryptedTokens []byte
	for {
		data, err := driver.GetToken(dialCtx, connID)
		if err == nil {
			encryptedTokens = data
			break
		}
		if !errors.Is(err, ErrNoData) {
			return nil, err
		}

		select {
		case <-dialCtx.Done():
			return nil, dialCtx.Err()
		case <-time.After(cfg.dataPoll):
		}
	}

	payload, err := noise.ReadMessage(encryptedTokens)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}

	var tokens SessionTokens
	if err := json.Unmarshal(payload, &tokens); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecodeTokenFailed, err)
	}

	if !noise.IsComplete() {
		return nil, ErrHandshakeIncomplete
	}

	transport, err := driver.NewTransport(dialCtx, connID, tokens, true)
	if err == nil {
		err = dialCtx.Err()
	}
	if err != nil {
		if transport != nil {
			err = errors.Join(err, disposeTransport(transport))
		}
		return nil, err
	}

	return newConn(cfg.ctx, cfg.cancel, transport, cfg, noise, driver, connID), nil
}

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

func (c *Conn) Read(p []byte) (n int, err error) {
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
			c.bufs.Dec = decrypted[:0]
			c.cleanupToken.Do(func() {
				if c.session != nil {
					go c.session.deleteToken()
				}
			})
			c.bufs.Read.Write(decrypted)
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

// Write reports bytes accepted into the connection, including when flushing
// fails. The connection owns those bytes and retains their ciphertext for retry;
// callers must only resubmit p[n:].
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
		return 0, c.ioError(ctx, c.flushContext(ctx))
	}
	accepted := 0
	for len(p) > 0 {
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
		for len(p) > 0 && available > FrameHeaderSize {
			size := min(len(p), c.mtu)
			if size+FrameHeaderSize > available {
				break
			}
			BuildFrame(&c.bufs.Write, Frame{Type: MsgTypeData, Payload: p[:size]})
			p = p[size:]
			accepted += size
			available -= size + FrameHeaderSize
		}
		c.wmu.Unlock()
		if err := c.flushContext(ctx); err != nil {
			return accepted, c.ioError(ctx, err)
		}
	}
	return accepted, nil
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
	BuildFrame(&c.bufs.Write, Frame{Type: MsgTypeFin})
	c.wmu.Unlock()

	return c.flush()
}

func (c *Conn) LocalAddr() net.Addr  { return c.transport.LocalAddr() }
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
					BuildFrame(&c.bufs.Write, Frame{Type: MsgTypePing})
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

func (c *Conn) flushContext(ctx context.Context) error {
	if err := lockIO(ctx, c.flushGate); err != nil {
		return err
	}
	defer func() { <-c.flushGate }()

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
		c.wmu.Unlock()
	}
	return nil
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

type metricsTransport struct {
	Transport
	rot Rotator // nil if underlying transport doesn't support rotation
	m   Metrics
}

type metricsLimitedTransport struct {
	*metricsTransport
	reader limitedRawReader
}

func newMetricsTransport(t Transport, m Metrics) Transport {
	mt := &metricsTransport{Transport: t, m: m}
	if r, ok := t.(Rotator); ok {
		mt.rot = r
	}
	if reader, ok := t.(limitedRawReader); ok {
		return &metricsLimitedTransport{metricsTransport: mt, reader: reader}
	}
	return mt
}

func (t *metricsTransport) WriteRaw(ctx context.Context, seq uint64, data io.ReadSeeker) error {
	var size int64
	if data != nil {
		pos, _ := data.Seek(0, io.SeekCurrent)
		end, _ := data.Seek(0, io.SeekEnd)
		_, _ = data.Seek(pos, io.SeekStart)
		size = end - pos
	}
	err := t.Transport.WriteRaw(ctx, seq, data)
	if err == nil {
		t.m.IncrementWriteTransaction()
		t.m.IncrementBytesSent(size)
	}
	return err
}

func (t *metricsTransport) ReadRaw(ctx context.Context) (io.ReadCloser, error) {
	rc, err := t.Transport.ReadRaw(ctx)
	return t.recordRead(rc, err)
}

func (t *metricsLimitedTransport) ReadRawLimit(ctx context.Context, limit int) (io.ReadCloser, error) {
	rc, err := t.reader.ReadRawLimit(ctx, limit)
	return t.recordRead(rc, err)
}

func (t *metricsTransport) recordRead(rc io.ReadCloser, err error) (io.ReadCloser, error) {
	if err == nil {
		t.m.IncrementReadTransaction()
		return &metricsReadCloser{ReadCloser: rc, m: t.m}, nil
	}
	return nil, err
}

func (t *metricsTransport) ShouldRotate() bool {
	if t.rot != nil {
		return t.rot.ShouldRotate()
	}
	return false
}

func (t *metricsTransport) RotateTX(ctx context.Context) error {
	if t.rot != nil {
		return t.rot.RotateTX(ctx)
	}
	return nil
}

func (t *metricsTransport) RotateRX() error {
	if t.rot != nil {
		return t.rot.RotateRX()
	}
	return nil
}

type metricsReadCloser struct {
	io.ReadCloser
	m Metrics
}

func (r *metricsReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.m.IncrementBytesReceived(int64(n))
	}
	return n, err
}
