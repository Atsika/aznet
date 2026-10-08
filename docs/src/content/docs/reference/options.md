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
The core poller switches to this interval after receiving data.

- **Default**: `10ms`
- **Use case**: Decrease for lower latency during active transfers; increase for cost savings.

## Lifecycle & Timeouts

### WithConnectTimeout

```go
func WithConnectTimeout(d time.Duration) Option
```

The maximum duration of client setup. On a Queue listener, it also sets token-message lifetime, rounded up to whole seconds. Expiring abandoned tokens bounds obstruction of later tokens by the first 32 visible messages; it does not guarantee that arbitrary bursts finish within each dialer's deadline. It does not shorten established session credentials. Zero or negative values leave the default unchanged.

- **Default**: `30s`

### WithIdleTimeout

```go
func WithIdleTimeout(d time.Duration) Option
```

The duration of inactivity before a connection is considered dead and
its Azure resources are eligible for cleanup by the server's janitor. Zero or negative values retain the default; they do not disable idle cleanup.

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

Legacy option setting both default bootstrap and session authorization duration. Use the independent lifetime APIs below when those policies differ.

- **Default**: `24h`
- **Security**: Session expiry can interrupt active connections. There is no automatic refresh.

## Advanced Configuration

### WithBufferLimits

```go
func WithBufferLimits(limits BufferLimits) Option
```

`BufferLimits` sets finite live-byte allowances per connection:

| Field | Default | Ownership |
| --- | --- | --- |
| `Pending` | 8 MiB | Received ciphertext; a separate allowance also bounds Queue reassembly and Table prefetch |
| `Decrypted` | 8 MiB | Decrypted framed bytes awaiting application reads |
| `Write` | 4 MiB | Accepted framed plaintext, including control frames |
| `Retry` | 4 MiB | One sealed outgoing chunk retained for an uncertain write |

`Write` returns once its bytes are queued in the `Write` allowance; a per-connection sender delivers them in order. Bytes queued while one storage request is in flight leave together as the next chunk, so small writes share requests. There is no coalescing timer: the first write on an idle connection is sent immediately.

- `Write` blocks only while the `Write` allowance is full. Concurrent writers wait interruptibly; write deadlines bound those waits, not a storage request already in flight.
- The returned count identifies bytes owned by the connection, including on error; retry only `p[n:]`.
- A failed background send is returned by the next `Write`, with `n == 0`. Failed chunks retain their exact ciphertext and sequence and are resent on the following flush; a failure recovered by that resend is not reported.
- `Write(nil)` and `CloseWrite()` wait until everything queued has been sent, and return any failure. `Close` allows only 250 ms for queued bytes, so call `CloseWrite` first when the peer must receive them.

The batching gain depends on how far application writes fall below the driver chunk size: large for Blob and Table, none for Queue. With 64 KiB writes on one live account (median of three), Blob rose from 1.74 to 8.76 MiB/s (34 to 1.9 requests/MiB), Table from 1.50 to 5.55 MiB/s (27 to 2.5 requests/MiB), and Queue stayed at 0.83 MiB/s. A connection now fills its `Write` allowance, so chunks and the receiver's buffers grow to several MiB: a Blob sender/receiver pair peaked at about 24 MB instead of 5 MB. Lowering `Write` to 1 MiB kept 6.9 MiB/s at a 9 MB peak. The effective MTU is also constrained by the write and retry allowances. One FIN header is reserved; redundant pings may be skipped when the buffer is full.

Receive overflow returns `ErrBufferLimit`, cancels connection I/O, and remains terminal on subsequent operations. Call `Close` to dispose of the transport and session. Smaller receive limits can reject valid larger chunks from a peer; configure both endpoints accordingly. Nonpositive fields leave defaults unchanged; `Write` must fit two frame headers plus one byte and `Retry` must fit a framed byte plus encryption overhead.

These are live-byte allowances, not a process-heap quota: buffer allocator capacity, encryption/decryption scratch, SDK JSON/base64 responses, and other connection state require additional finite memory. They do not bound unread cloud storage or implement ProxyBlob logical-stream flow control.

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

## Independent credential lifetimes

`WithSessionDuration(d)` sets the authorization duration of newly created sessions;
the default remains 24 hours. It does not change default bootstrap validity or
renew already issued credentials. `Listener.ConnectionStringFor(d)` issues one
bootstrap URL for the requested duration without mutating listener policy, and may
run concurrently with `Accept` or other connection-string calls. Expiry is computed
when credentials are signed, not when a connection becomes visible.

The existing `WithSASExpiry(d)` remains a legacy option setting both default
bootstrap and session durations. Prefer the independent APIs for new callers.
Option order follows the usual last-setting-wins rule; `WithSessionDuration` changes
only session duration. `ConnectionString()` retains the configured default bootstrap
duration. Durations shorter than one second, including zero/negative values, fail
with `ErrInvalidConfig`, instead of silently falling back to defaults. Fractional
seconds are represented at the Azure SAS timestamp's whole-second precision.

```go
listener, err := aznet.Listen(network, address,
    aznet.WithSessionDuration(24*time.Hour))
// Handle err before using listener.
bootstrap, err := listener.(*aznet.Listener).ConnectionStringFor(7*24*time.Hour)
```

`BootstrapTokenIssuer` is an optional driver capability. Custom drivers can retain
the original Driver method set: their ordinary `ConnectionString()` still works;
explicit per-string duration returns `ErrBootstrapDurationUnsupported` until the
capability is implemented. Implementations must not mutate shared configuration to
satisfy a single issuance request.
