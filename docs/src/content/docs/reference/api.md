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

Handles the full connection lifecycle: handshake posting/polling, token exchange, session creation, and transport instantiation.

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
- `Write(b []byte) (n int, err error)`
- `Close() error`
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
- `Close() error`: Gracefully closes all active connections and removes shared bootstrap endpoints from Azure Storage.
