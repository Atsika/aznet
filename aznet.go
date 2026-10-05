package aznet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sort"
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

func (a ServiceAddr) String() string { return a.Endpoint + "/" + a.Resource }

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
