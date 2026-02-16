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
	// WriteRaw sends raw bytes to the peer.
	WriteRaw(ctx context.Context, data io.ReadSeeker) error
	// ReadRaw attempts to read raw bytes from the peer.
	ReadRaw(ctx context.Context) (io.ReadCloser, error)
	// Close terminates the transport.
	Close() error
	// LocalAddr returns the local network address.
	LocalAddr() net.Addr
	// RemoteAddr returns the remote network address.
	RemoteAddr() net.Addr
	// MaxRawSize returns the maximum raw capacity of the transport in bytes.
	MaxRawSize() int
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

	// Session lifecycle
	CreateSession(ctx context.Context, connID string) (SessionTokens, error)
	CreateBootstrapTokens() (hSAS, tSAS string, err error)

	// NewTransport creates a data transporter for an established session.
	NewTransport(ctx context.Context, connID string, tokens SessionTokens, isInitiator bool) (Transport, error)

	// CleanupBootstrap removes shared bootstrap resources (handshake/token endpoints).
	CleanupBootstrap(ctx context.Context) error
	// CleanupSession removes per-connection resources (req/res channels).
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
		return nil, nil, nil, err
	}

	u, err := url.Parse(address)
	if err != nil {
		return nil, nil, nil, err
	}
	ep := NewEndpoint(u)

	driver, err := factory.NewDriver(ep, cfg)
	if err != nil {
		return nil, nil, nil, err
	}

	return &metricsDriver{Driver: driver, m: cfg.metrics}, ep, cfg, nil
}

// Listen is analogous to net.Listen. It takes a network type (e.g. "azblob")
// and an address (e.g. "account.blob.core.windows.net").
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
func Dial(network, address string, opts ...Option) (net.Conn, error) {
	driver, _, cfg, err := initialize(network, address, opts)
	if err != nil {
		return nil, err
	}

	connID := uuid.New().String()
	noise, err := NewNoiseClient()
	if err != nil {
		return nil, err
	}
	msg1, err := noise.WriteMessage([]byte(connID))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoiseMsgFailed, err)
	}

	if err := driver.PostHandshake(cfg.ctx, connID, msg1); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshakeExchangeFailed, err)
	}

	dialCtx, dialCancel := context.WithTimeout(cfg.ctx, cfg.connectTimeout)
	defer dialCancel()

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

	transport, err := driver.NewTransport(cfg.ctx, connID, tokens, true)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(cfg.ctx)
	return newConn(ctx, cancel, transport, cfg, noise, driver, connID), nil
}

// Conn implements net.Conn.
type Conn struct {
	transport Transport
	rotator   Rotator // nil if transport doesn't support rotation
	driver    Driver
	ctx       context.Context
	cancel    context.CancelFunc

	bufs  *Buffers
	cfg   *Config
	noise *Noise
	poll  *AdaptivePoll

	readDeadline  atomic.Pointer[time.Time]
	writeDeadline atomic.Pointer[time.Time]

	id string

	lastActive   atomic.Int64
	peerLastSeen atomic.Int64

	cleanupToken sync.Once
	closeOnce    sync.Once
	// wmu guards the write buffer (bufs.Write). Acquired briefly inside flush()
	// to drain the buffer, then released before the transport.WriteRaw call.
	wmu sync.Mutex
	// rmu guards the read buffer (bufs.Read), readRemain, and the Noise decryption
	// buffer. Never held while calling transport methods.
	rmu sync.Mutex
	// fmu serializes flush() calls so only one goroutine encrypts and sends at a
	// time. Lock order: fmu → wmu (never reverse).
	fmu sync.Mutex

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
		bufs:      buffersPool.Get().(*Buffers),
		mtu:       t.MaxRawSize() - NoiseOverhead - FrameHeaderSize,
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

func (c *Conn) Read(p []byte) (int, error) {
	for {
		if c.closed.Load() == 1 {
			return 0, net.ErrClosed
		}

		c.rmu.Lock()
		if c.closedRead.Load() == 1 {
			c.rmu.Unlock()
			return 0, io.EOF
		}

		deadline := c.readDeadline.Load()
		if deadline != nil && !deadline.IsZero() && time.Now().After(*deadline) {
			c.rmu.Unlock()
			return 0, os.ErrDeadlineExceeded
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
			fLen := int(binary.BigEndian.Uint32(header[:4]))

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

		c.rmu.Unlock()

		// Fetch more data
		rawStream, err := c.transport.ReadRaw(c.ctx)
		if err != nil {
			if errors.Is(err, ErrNoData) {
				c.poll.Sleep()
				continue
			}
			if errors.Is(err, context.Canceled) {
				if c.closed.Load() == 1 {
					return 0, net.ErrClosed
				}
			}
			return 0, err
		}

		// Read directly from the stream into the Noise buffer.
		_, err = c.bufs.Noise.ReadFrom(rawStream)
		rawStream.Close()
		if err != nil && err != io.EOF {
			return 0, err
		}

		// Decrypt and process
		c.rmu.Lock()
		for {
			decrypted, rest, err := c.noise.UnsealData(c.bufs.Dec, c.bufs.Noise.Bytes())
			if err != nil {
				if err != io.ErrShortBuffer {
					c.rmu.Unlock()
					return 0, err
				}
				break
			}

			c.bufs.Dec = decrypted[:0]

			c.cleanupToken.Do(func() {
				if !c.noise.IsInitiator() && c.driver != nil {
					go func() {
						ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
						defer cancel()
						_ = c.driver.DeleteToken(ctx, c.id)
					}()
				}
			})

			c.bufs.Read.Write(decrypted)
			used := c.bufs.Noise.Len() - len(rest)
			c.bufs.Noise.Next(used)
		}
		c.rmu.Unlock()
		c.poll.Reset()
	}
}

func (c *Conn) Write(p []byte) (int, error) {
	if c.closed.Load() == 1 || c.closedWrite.Load() == 1 {
		return 0, io.ErrClosedPipe
	}
	deadline := c.writeDeadline.Load()
	if deadline != nil && !deadline.IsZero() && time.Now().After(*deadline) {
		return 0, os.ErrDeadlineExceeded
	}

	total := len(p)
	c.wmu.Lock()
	for len(p) > 0 {
		chunkSize := min(len(p), int(c.mtu))
		BuildFrame(&c.bufs.Write, Frame{Type: MsgTypeData, Payload: p[:chunkSize]})
		p = p[chunkSize:]
	}
	c.wmu.Unlock()

	if err := c.flush(); err != nil {
		return 0, err
	}
	return total, nil
}

func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.closed.Store(1)
		_ = c.flush()

		if c.closedWrite.Load() == 0 {
			c.wmu.Lock()
			BuildFrame(&c.bufs.Write, Frame{Type: MsgTypeFin})
			c.wmu.Unlock()
		}

		_ = c.flush()
		err = c.transport.Close()
		c.cancel()

		if c.bufs != nil {
			c.bufs.Read.Reset()
			c.bufs.Write.Reset()
			c.bufs.Noise.Reset()
			c.bufs.Enc = c.bufs.Enc[:0]
			c.bufs.Dec = c.bufs.Dec[:0]
			buffersPool.Put(c.bufs)
			c.bufs = nil
		}
	})
	return err
}

// CloseWrite shuts down the writing side of the connection. It sends a FIN frame
// to the peer to indicate that no more data will be sent.
func (c *Conn) CloseWrite() error {
	if c.closed.Load() == 1 || c.closedWrite.Swap(1) == 1 {
		return nil
	}
	c.wmu.Lock()
	BuildFrame(&c.bufs.Write, Frame{Type: MsgTypeFin})
	c.wmu.Unlock()

	return c.flush()
}

func (c *Conn) LocalAddr() net.Addr  { return c.transport.LocalAddr() }
func (c *Conn) RemoteAddr() net.Addr { return c.transport.RemoteAddr() }

func (c *Conn) SetDeadline(t time.Time) error {
	c.readDeadline.Store(&t)
	c.writeDeadline.Store(&t)
	return nil
}

func (c *Conn) SetReadDeadline(t time.Time) error {
	c.readDeadline.Store(&t)
	return nil
}

func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.writeDeadline.Store(&t)
	return nil
}

// MTU returns the maximum number of application bytes that can fit in a single
// transport frame for the current connection.
func (c *Conn) MTU() int {
	return c.mtu
}

func (c *Conn) GetMetrics() Metrics { return c.cfg.metrics }

// keepAlive sends periodic Ping frames when the local side is idle.
// lastActive tracks the time of the most recent flush (local send).
// peerLastSeen (updated in Read) tracks the most recent received frame.
// A Ping is only sent if no data was flushed within the pingInterval.
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
				BuildFrame(&c.bufs.Write, Frame{Type: MsgTypePing})
				c.wmu.Unlock()
				_ = c.flush()
				continue
			}
		}
	}
}

func (c *Conn) flush() error {
	c.fmu.Lock()
	defer c.fmu.Unlock()

	maxChunk := c.transport.MaxRawSize() - NoiseOverhead

	for {
		c.wmu.Lock()
		if c.bufs.Write.Len() == 0 {
			c.wmu.Unlock()
			return nil
		}

		// Check rotation while holding lock
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

			if err := c.transport.WriteRaw(c.ctx, bytes.NewReader(sealed)); err != nil {
				return err
			}
			if err := c.rotator.RotateTX(c.ctx); err != nil {
				return err
			}
			continue // Re-check buffer after rotation
		}

		takeLen := min(c.bufs.Write.Len(), int(maxChunk))
		plaintext := c.bufs.Write.Next(takeLen)
		c.wmu.Unlock()

		sealed, err := c.noise.SealData(c.bufs.Enc, plaintext)
		if err != nil {
			return err
		}

		c.bufs.Enc = sealed[:0]

		err = c.transport.WriteRaw(c.ctx, bytes.NewReader(sealed))
		if err != nil {
			return err
		}

		c.lastActive.Store(time.Now().UnixNano())
	}
}

// Listener implements net.Listener.
type Listener struct {
	network string
	ep      *Endpoint
	driver  Driver
	cfg     *Config
	conns   sync.Map // map[string]*Conn
}

func (l *Listener) Accept() (net.Conn, error) {
	for {
		select {
		case <-l.cfg.ctx.Done():
			return nil, net.ErrClosed
		default:
		}

		handshakes, err := l.driver.GetHandshakes(l.cfg.ctx)
		if err != nil {
			time.Sleep(l.cfg.acceptPoll)
			continue
		}

		for _, hs := range handshakes {
			noise, err := NewNoiseServer()
			if err != nil {
				continue
			}
			payload, err := noise.ReadMessage(hs.Payload)
			if err != nil {
				continue
			}

			// The payload contains the actual connID from the client.
			connID := string(payload)
			if connID == "" {
				continue
			}

			// Check if we already have this connection
			if _, ok := l.conns.Load(connID); ok {
				continue
			}

			// Generate tokens (driver specific tokens via Provider)
			tokens, err := l.driver.CreateSession(l.cfg.ctx, connID)
			if err != nil {
				continue
			}
			encodedTokens, err := json.Marshal(tokens)
			if err != nil {
				continue
			}

			msg2, err := noise.WriteMessage(encodedTokens)
			if err != nil {
				continue
			}

			if err := l.driver.PostToken(l.cfg.ctx, connID, msg2); err != nil {
				continue
			}

			if !noise.IsComplete() {
				continue
			}

			// Inform Provider we are done and want a transport
			transport, err := l.driver.NewTransport(l.cfg.ctx, connID, tokens, false)
			if err != nil {
				continue
			}

			_ = l.driver.DeleteHandshake(l.cfg.ctx, hs.ID)
			ctx, cancel := context.WithCancel(l.cfg.ctx)
			conn := newConn(ctx, cancel, transport, l.cfg, noise, l.driver, connID)
			l.conns.Store(connID, conn)
			return conn, nil
		}
		time.Sleep(l.cfg.acceptPoll)
	}
}

// ConnectionString returns the connection string for this listener.
func (l *Listener) ConnectionString() (string, error) {
	hSAS, tSAS, err := l.driver.CreateBootstrapTokens()
	if err != nil {
		return "", err
	}

	return l.ep.BuildConnURL(l.cfg, hSAS, tSAS), nil
}

func (l *Listener) Close() error {
	l.cfg.cancel()

	// Gracefully close all connections
	l.conns.Range(func(key, value any) bool {
		conn := value.(*Conn)
		_ = conn.Close()
		return true
	})

	// Cleanup shared bootstrap endpoints
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return l.driver.CleanupBootstrap(ctx)
}

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
				id := key.(string)
				conn := value.(*Conn)

				closed := conn.closed.Load() == 1
				closedRead := conn.closedRead.Load() == 1
				peerLastSeen := time.Unix(0, conn.peerLastSeen.Load())

				if (closed && closedRead) || time.Since(peerLastSeen) > l.cfg.idleTimeout {
					_ = conn.Close()
					// Final cleanup of driver resources
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					_ = l.driver.DeleteToken(ctx, id)
					_ = l.driver.CleanupSession(ctx, id)
					cancel()
					l.conns.Delete(id)
				}
				return true
			})
		}
	}
}

type metricsTransport struct {
	Transport
	rot Rotator // nil if underlying transport doesn't support rotation
	m   Metrics
}

func newMetricsTransport(t Transport, m Metrics) *metricsTransport {
	mt := &metricsTransport{Transport: t, m: m}
	if r, ok := t.(Rotator); ok {
		mt.rot = r
	}
	return mt
}

func (t *metricsTransport) WriteRaw(ctx context.Context, data io.ReadSeeker) error {
	var size int64
	if data != nil {
		pos, _ := data.Seek(0, io.SeekCurrent)
		end, _ := data.Seek(0, io.SeekEnd)
		_, _ = data.Seek(pos, io.SeekStart)
		size = end - pos
	}
	err := t.Transport.WriteRaw(ctx, data)
	if err == nil {
		t.m.IncrementWriteTransaction()
		t.m.IncrementBytesSent(size)
	}
	return err
}

func (t *metricsTransport) ReadRaw(ctx context.Context) (io.ReadCloser, error) {
	rc, err := t.Transport.ReadRaw(ctx)
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
