---
title: Configuration Options
description: Detailed guide to all functional options for configuring aznet listeners and connections.
---

`aznet` uses functional options to configure behavior. These options can be passed to both `Listen` and `Dial`.

## Polling Options

### WithDataPoll

```go
func WithDataPoll(d time.Duration) Option
```

Sets the maximum interval between polling requests for new data.

- **Default**: `500ms`
- **Use case**: Increase for better battery/cost efficiency; decrease for lower latency.

### WithAcceptPoll

```go
func WithAcceptPoll(d time.Duration) Option
```

Sets how frequently a `net.Listener` scans for new connection handshakes.

- **Default**: `1s`
- **Use case**: Decrease if you expect many simultaneous connection attempts.

### WithFastPoll

```go
func WithFastPoll(d time.Duration) Option
```

Sets a faster polling interval used when data is actively flowing.
Drivers switch to this interval immediately after receiving a chunk.

- **Default**: `10ms`
- **Use case**: Decrease for lower latency during active transfers; increase for cost savings.

## Lifecycle & Timeouts

### WithConnectTimeout

```go
func WithConnectTimeout(d time.Duration) Option
```

The maximum time `Dial` will wait for the server to acknowledge the handshake.

- **Default**: `30s`

### WithIdleTimeout

```go
func WithIdleTimeout(d time.Duration) Option
```

The duration of inactivity before a connection is considered dead and
its Azure resources are eligible for cleanup by the server's janitor.

- **Default**: `5m`

### WithPing

```go
func WithPing(d time.Duration) Option
```

The interval between keep-alive "Ping" frames. Set to `0` to disable.

- **Default**: `30s`

### WithSASExpiry

```go
func WithSASExpiry(d time.Duration) Option
```

The duration for which generated Shared Access Signature (SAS) tokens remain valid.

- **Default**: `24h`
- **Security**: Shorter expiries are safer but may interrupt long-running connections if not refreshed.

## Advanced Configuration

### WithContext

```go
func WithContext(ctx context.Context) Option
```

Provides a parent context for all operations. Closing this context will terminate the listener or connection.

### WithMetrics

```go
func WithMetrics(metrics Metrics) Option
```

Injects a custom metrics implementation. See [Metrics Reference](/reference/metrics) for details.

### WithPrefixes

```go
func WithPrefixes(reqPrefix, resPrefix string) Option
```

Overrides the default prefixes (`req` and `res`) used for Azure resource naming (blobs, queues, or tables).

### WithEndpoints

```go
func WithEndpoints(handshake, token string) Option
```

Overrides the default endpoint names (`handshake` and `token`) used during connection bootstrap.
