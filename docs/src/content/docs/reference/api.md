---
title: Core API Reference
description: Detailed documentation for the main aznet package functions and interfaces.
---

The `aznet` package provides a standard Go networking interface over Azure Storage.

## Core Functions

### Listen

```go
func Listen(network, address string, opts ...Option) (net.Listener, error)
```

`Listen` is analogous to `net.Listen`. It starts a listener that polls an Azure Storage resource for incoming connection requests.

- **network**: The driver type to use (e.g., `"azblob"`, `"azqueue"`, `"aztable"`).
- **address**: A URL or host identifying the Azure resource (e.g., `https://account.blob.core.windows.net`).
- **opts**: Optional functional options to configure the listener.
- **Returns**: A `net.Listener` implementation.

### Dial

```go
func Dial(network, address string, opts ...Option) (net.Conn, error)
```

`Dial` is analogous to `net.Dial`. It establishes a connection to a remote `aznet.Listener` by performing a Noise handshake via Azure Storage.

- **network**: The driver type to use (e.g., `"azblob"`, `"azqueue"`, `"aztable"`).
- **address**: A URL provided by the server or generated via `azurl`.
- **opts**: Optional functional options to configure the connection.
- **Returns**: A `net.Conn` implementation.

## Driver Registration

### RegisterFactory

```go
func RegisterFactory(scheme string, factory Factory)
```

Registers a new driver factory for a specific URL scheme (e.g., `"azblob"`). This is typically called in an `init()` function by driver packages. Panics if a factory is already registered for the given scheme.

### GetFactories

```go
func GetFactories() []string
```

Returns a sorted list of currently registered driver schemes.

## Interfaces

### Factory

```go
type Factory interface {
    NewDriver(ep *Endpoint, cfg *Config) (Driver, error)
}
```

Creates a `Driver` from a parsed endpoint and configuration. Each driver package registers a `Factory` via `RegisterFactory()` in its `init()` function.

### Driver

```go
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
```

Handles the full connection lifecycle: handshake posting/polling, token exchange, session creation, and transport instantiation. The listener owns the session ID before calling `CreateSession` and calls `CleanupSession` even after partial acquisition failure. Custom drivers must make resource deletion idempotent, report failures, and honor operation contexts. If `NewTransport` returns a partial transport with an error, aznet closes that transport once.

### Transport

```go
type Transport interface {
    WriteRaw(ctx context.Context, data io.ReadSeeker) error
    ReadRaw(ctx context.Context) (io.ReadCloser, error)
    Close() error
    LocalAddr() net.Addr
    RemoteAddr() net.Addr
    MaxRawSize() int
}
```

The raw byte-exchange interface implemented by drivers for data transfer.

### Rotator

```go
type Rotator interface {
    ShouldRotate() bool
    RotateTX(ctx context.Context) error
    RotateRX() error
}
```

Optionally implemented by transports that need resource rotation (e.g., blob append blobs have a 50,000 block limit). The core handles rotation signaling automatically when a `Transport` also satisfies this interface.

`aznet.Conn` (returned by `Dial` or `Accept`) implements the standard `net.Conn` interface:

- `Read(b []byte) (n int, err error)`
- `Write(b []byte) (n int, err error)`: Reports bytes accepted into the connection, including when flushing fails. Retry only `b[n:]`; aznet retains pending ciphertext for retry.
- `Close() error`: Attempts buffered data and FIN for up to 250 ms, cancels I/O, and closes the transport. Accepted connections also delete their session resources with bounded cleanup. Repeated calls return the original result.
- `LocalAddr() net.Addr`
- `RemoteAddr() net.Addr`
- `SetDeadline(t time.Time) error`
- `SetReadDeadline(t time.Time) error`
- `SetWriteDeadline(t time.Time) error`
- `MTU() int`: Returns the maximum application payload size for a single frame.
- `CloseWrite() error`: Shuts down the writing side of the connection (half-close).
- `GetMetrics() Metrics`: Returns the connection's metrics tracker.

The `net.Listener` implementation returned by `Listen` also provides:

- `ConnectionString() (string, error)`: Returns a connection URL with embedded SAS tokens that can be shared with clients.
- `Accept() (net.Conn, error)`: Retains unprocessed batch entries. Empty polls wait normally; backend and setup failures return `*aznet.AcceptError`, whose `Op`, `Unwrap`, `Temporary`, and `Timeout` methods let callers inspect and classify failures. Callers own retry/backoff decisions.
- `Close() error`: Cancels acceptance and closes all owned sessions, independently of the janitor. Returns cleanup failures or a timeout if teardown remains incomplete. Repeated calls return the original result. Shared bootstrap endpoints remain intact.
- `CleanupBootstrap(ctx context.Context) error`: Explicitly deletes the shared handshake/token namespace. Its administrator should call this only after every namespace user has stopped, with an uncancelled cleanup context. Deletion failures are returned.

### Closing and resource ownership

A successful storage upload is not an acknowledgement that the peer consumed the bytes. Full `Close` on an accepted connection deletes session storage and can discard data the peer has not read. For delivery-sensitive responses, call `CloseWrite`, wait for application-level completion from the peer, then call `Close`.

Each session cleanup operation has a two-second deadline and at most three core attempts for transient errors; SDK attempts share that deadline. Partial transports are closed once. Connection Close retains its 250 ms graceful bound followed by bounded token and session cleanup. Listener Close returns within 10.25 s. A custom backend that ignores cancellation can retain its worker until it returns; a timeout does not confirm resource reclamation.

**Migration:** listener Close previously removed shared bootstrap resources. Namespace administrators must now request `CleanupBootstrap` explicitly. This prevents session teardown from destroying discovery resources shared with another listener generation. These additional methods are available on `*aznet.Listener`, obtained by type assertion from the `net.Listener` returned by `Listen`.

## Session authorization expiry

`GetSessionExpiry(conn)` returns `(time.Time, bool)` through the optional connection
`SessionExpiry` capability. False means unknown. Built-in adapters record the exact
earliest expiration encoded in the required request/response credentials and carry
it in the encrypted session-token exchange, so both the accepted and dialed
connection report the same timestamp. Callers do not need access to credentials.

Custom drivers may populate `SessionTokens.ExpiresAt` with trustworthy issuance
metadata. Its zero value means unknown and is omitted from the exchange. Older
peers that omit expiry also yield unknown; no connection-time estimate is invented.
The original Driver/Transport interfaces remain unchanged.

Expiry is informational. There is no automatic renewal, expiry timer, or forced
close at the displayed timestamp. Bootstrap expiry governs discovery/joining; it
does not by itself expire an established session. Session authorization can fail
for other reasons or stop at a backend-specific boundary. SDK errors remain
inspectable with `errors.As`/`errors.Is`: an authorization or permission failure is
not automatically relabeled as expiration. In particular, `AuthenticationFailed`
may describe signature validity while `AuthorizationPermissionMismatch` identifies
permissions. Inspect the preserved service response rather than infer cause from
HTTP 403 or a countdown alone. Metadata is not a liveness guarantee.
