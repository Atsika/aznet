package aznet

import (
	"context"
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

	sasExpiry time.Duration

	fastPoll time.Duration
	dataPoll time.Duration

	acceptPoll   time.Duration
	pingInterval time.Duration

	connectTimeout time.Duration
	idleTimeout    time.Duration
}

// Validate checks if the configuration is sane and valid.
func (c *Config) Validate() error {
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
		fastPoll:          DefaultFastPoll,
		dataPoll:          DefaultDataPoll,
		acceptPoll:        DefaultAcceptPoll,
		pingInterval:      DefaultPingInterval,
		connectTimeout:    DefaultConnectTimeout,
		idleTimeout:       DefaultIdleTimeout,
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

// SASTimes returns the start and end times for a SAS token based on config.
func (c *Config) SASTimes() (start, end time.Time) {
	now := time.Now().UTC()
	return now.Add(-5 * time.Minute), now.Add(c.sasExpiry)
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

// WithSASExpiry sets the validity time for SAS tokens. The token cannot be revoked
// once generated, so be careful and don't set it too long.
func WithSASExpiry(d time.Duration) Option {
	return func(c *Config) {
		if d > 0 {
			c.sasExpiry = d
		}
	}
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

// WithConnectTimeout sets the maximum duration the client waits for the listener to acknowledge
// a Dialled connection. Zero or negative disables the timeout.
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
