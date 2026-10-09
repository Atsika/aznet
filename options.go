package aznet

import (
	"context"
	"fmt"
	"time"
)

const (
	// Default endpoint names for connection bootstrap
	DefaultHandshakeEndpoint = "handshake"
	DefaultTokenEndpoint     = "token"

	// DefaultReqPrefix is the default prefix for request channels.
	DefaultReqPrefix = "req"
	// DefaultResPrefix is the default prefix for response channels.
	DefaultResPrefix = "res"

	// DefaultSASExpiry is the default authorization expiry time.
	DefaultSASExpiry = 24 * time.Hour

	// DefaultFastPoll is the polling interval used during activity.
	// Adaptive polling backs off exponentially from FastPoll to DataPoll.
	DefaultFastPoll = 10 * time.Millisecond
	// DefaultDataPoll is the steady-state polling interval for idle connections.
	// At 500ms this produces ~7,200 read API calls per hour per connection.
	// Tune via WithDataPoll() to balance latency vs cost.
	DefaultDataPoll = 500 * time.Millisecond
	// DefaultAcceptPoll is the polling interval for listeners accepting incoming connections.
	DefaultAcceptPoll = 1 * time.Second
	// DefaultPingInterval is the interval between keep-alive heartbeats.
	DefaultPingInterval = 30 * time.Second

	// DefaultConnectTimeout is the maximum duration the client waits for connection acknowledgment.
	DefaultConnectTimeout = 30 * time.Second
	// DefaultIdleTimeout is the idle timeout before considering a peer dead.
	DefaultIdleTimeout = 5 * time.Minute
)

// Option defines a functional option for Listen/Dial.
type Option func(*Config)

// Config holds runtime settings for a connection or listener. Zero value
// yields sane defaults via defaultConfig(). Users should modify it through
// functional options.
type Config struct {
	ctx     context.Context
	metrics Metrics
	cancel  context.CancelFunc

	tokenEndpoint     string
	handshakeEndpoint string
	reqPrefix         string
	resPrefix         string

	sasExpiry       time.Duration
	sessionDuration time.Duration
	now             func() time.Time

	fastPoll time.Duration
	dataPoll time.Duration

	acceptPoll   time.Duration
	pingInterval time.Duration

	connectTimeout time.Duration
	idleTimeout    time.Duration
	bufferLimits   BufferLimits
}

// Validate checks if the configuration is sane and valid.
func (c *Config) Validate() error {
	if err := validateCredentialDuration(c.sasExpiry); err != nil {
		return err
	}
	if err := validateCredentialDuration(c.sessionDuration); err != nil {
		return err
	}
	if c.handshakeEndpoint == c.tokenEndpoint {
		return ErrInvalidConfig
	}
	if c.reqPrefix == c.resPrefix {
		return ErrInvalidConfig
	}
	return nil
}

// defaultConfig returns config with library defaults.
func defaultConfig() *Config {
	ctx, cancel := context.WithCancel(context.Background())
	return &Config{
		ctx:               ctx,
		cancel:            cancel,
		metrics:           NewDefaultMetrics(),
		handshakeEndpoint: DefaultHandshakeEndpoint,
		tokenEndpoint:     DefaultTokenEndpoint,
		reqPrefix:         DefaultReqPrefix,
		resPrefix:         DefaultResPrefix,
		sasExpiry:         DefaultSASExpiry,
		sessionDuration:   DefaultSASExpiry,
		now:               time.Now,
		fastPoll:          DefaultFastPoll,
		dataPoll:          DefaultDataPoll,
		acceptPoll:        DefaultAcceptPoll,
		pingInterval:      DefaultPingInterval,
		connectTimeout:    DefaultConnectTimeout,
		idleTimeout:       DefaultIdleTimeout,
		bufferLimits:      DefaultBufferLimits(),
	}
}

// applyConfig builds a runtime config by applying the given options on top of defaults.
func applyConfig(opts []Option) *Config {
	cfg := defaultConfig()
	for _, o := range opts {
		o(cfg)
	}
	return cfg
}

// SASTimes returns issuance times for session credentials. Custom drivers can
// use this policy while exposing unknown expiry if they cannot report exact issuance.
func (c *Config) SASTimes() (start, end time.Time) {
	return c.sasTimes(c.sessionDuration)
}

func (c *Config) sasTimes(duration time.Duration) (start, end time.Time) {
	now := time.Now().UTC()
	if c.now != nil {
		now = c.now().UTC()
	}
	return now.Add(-5 * time.Minute), now.Add(duration)
}

// WithEndpoints allows overriding the default handshake and token endpoints
// used during the connection bootstrap phase.
func WithEndpoints(handshake, token string) Option {
	return func(c *Config) {
		if handshake != "" {
			c.handshakeEndpoint = handshake
		}
		if token != "" {
			c.tokenEndpoint = token
		}
	}
}

// WithPrefixes sets the prefixes that drivers use when creating per-connection
// request/response artefacts (e.g. blobs or queues).
func WithPrefixes(reqPrefix, resPrefix string) Option {
	return func(c *Config) {
		if reqPrefix != "" {
			c.reqPrefix = reqPrefix
		}
		if resPrefix != "" {
			c.resPrefix = resPrefix
		}
	}
}

// WithSASExpiry sets both legacy default bootstrap and session durations.
// Prefer WithSessionDuration plus ConnectionStringFor for independent policies.
// Durations below one second fail configuration validation.
func WithSASExpiry(d time.Duration) Option {
	return func(c *Config) {
		c.sasExpiry = d
		c.sessionDuration = d
	}
}

// WithSessionDuration sets the validity of newly issued session credentials.
// It does not change default bootstrap validity or existing sessions. The default
// is 24 hours. Durations below one second fail configuration validation.
func WithSessionDuration(d time.Duration) Option {
	return func(c *Config) { c.sessionDuration = d }
}

// WithAcceptPoll sets how frequently the listener scans for new connections.
func WithAcceptPoll(d time.Duration) Option {
	return func(c *Config) {
		if d > 0 {
			c.acceptPoll = d
		}
	}
}

// WithFastPoll sets the polling interval used when data is actively flowing.
func WithFastPoll(d time.Duration) Option {
	return func(c *Config) {
		if d > 0 {
			c.fastPoll = d
		}
	}
}

// WithDataPoll sets how often established connections poll for data.
func WithDataPoll(d time.Duration) Option {
	return func(c *Config) {
		if d > 0 {
			c.dataPoll = d
		}
	}
}

// WithPing sets the keep-alive heartbeat cadence. Zero disables keep-alive.
func WithPing(d time.Duration) Option {
	return func(c *Config) {
		if d >= 0 {
			c.pingInterval = d
		}
	}
}

// WithConnectTimeout bounds client setup and the lifetime of Queue tokens issued
// by a listener. Zero or negative values leave the default unchanged.
func WithConnectTimeout(d time.Duration) Option {
	return func(c *Config) {
		if d > 0 {
			c.connectTimeout = d
		}
	}
}

// WithIdleTimeout sets the grace period after which background janitors purge half-closed
// connections that never completed a FIN handshake. Zero disables automatic cleanup.
func WithIdleTimeout(d time.Duration) Option {
	return func(c *Config) {
		if d > 0 {
			c.idleTimeout = d
		}
	}
}

// WithContext sets the base context for all network/SDK calls initiated by
// Listen/Dial. Useful for cancellation or shared tracing.
func WithContext(ctx context.Context) Option {
	return func(c *Config) {
		if ctx != nil {
			c.ctx, c.cancel = context.WithCancel(ctx)
		}
	}
}

// WithMetrics sets a custom metrics implementation for tracking connection statistics.
// If not provided, a default implementation with atomic counters will be used.
func WithMetrics(metrics Metrics) Option {
	return func(c *Config) {
		if metrics != nil {
			c.metrics = metrics
		}
	}
}

// WithBufferLimits replaces positive allowances. Write reserves one FIN header;
// Write and Retry must each fit a framed byte (plus encryption for Retry).
// Smaller receive allowances may reject a peer's larger chunks terminally.
func WithBufferLimits(limits BufferLimits) Option {
	return func(c *Config) {
		if limits.Pending > 0 {
			c.bufferLimits.Pending = limits.Pending
		}
		if limits.Decrypted > 0 {
			c.bufferLimits.Decrypted = limits.Decrypted
		}
		if limits.Write >= 2*FrameHeaderSize+1 {
			c.bufferLimits.Write = limits.Write
		}
		if limits.Retry >= NoiseOverhead+FrameHeaderSize+1 {
			c.bufferLimits.Retry = limits.Retry
		}
		if limits.WriteChunks > 0 {
			c.bufferLimits.WriteChunks = limits.WriteChunks
		}
	}
}

// BufferLimits bounds live bytes per connection in each ownership stage.
// Pending bounds received ciphertext and, separately, Queue reassembly.
// Decrypted bounds framed plaintext awaiting Read. Write bounds queued framed
// plaintext (including control frames). WriteChunks, when positive, further
// caps Write at that many of the transport's chunks: queued bytes leave one
// chunk at a time, so a small write waits behind every chunk queued ahead of
// it. Retry bounds one sealed outgoing chunk.
// Encryption/decryption scratch and allocator capacity are additional, finite
// storage; these allowances are not a process heap or cloud storage quota.
type BufferLimits struct {
	Pending, Decrypted, Write, Retry, WriteChunks int
}

// DefaultBufferLimits accommodates two maximum Blob chunks on receive, and one
// on retry. Large application writes are flushed in bounded batches.
func DefaultBufferLimits() BufferLimits {
	return BufferLimits{Pending: 8 << 20, Decrypted: 8 << 20, Write: 4 << 20, Retry: 4 << 20}
}

func (c *Config) limits() BufferLimits {
	if c == nil || c.bufferLimits.Pending == 0 {
		return DefaultBufferLimits()
	}
	return c.bufferLimits
}

func validateCredentialDuration(duration time.Duration) error {
	if duration < time.Second {
		return fmt.Errorf("%w: credential duration must be at least one second", ErrInvalidConfig)
	}
	return nil
}
