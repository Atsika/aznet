# aznet

<p align="center">
    <img src="./docs/src/assets/aznet.png" width="300">
<p>

<p align="center"><i>Network abstraction over Azure Storage</i></p>

The standard Go `net.Conn` interface using Azure Storage services as the transport layer.

## What is aznet?

aznet provides a TCP-like networking abstraction over Azure Storage, allowing you to use familiar socket programming patterns with cloud storage as the underlying transport. Any code that works with `net.Conn` can work with aznet.

### Drop-in Replacement Example

Swap standard TCP for Azure Storage with a single line:

```go
// Before: Standard TCP
ln, _ := net.Listen("tcp", ":8080")

// After: aznet using Azure Blob Storage
ln, _ := aznet.Listen("azblob", "https://account:key@account.blob.core.windows.net/")
```

## Documentation

Comprehensive documentation is available in the [`docs/`](docs/) directory and rendered via Starlight:

```bash
cd docs
pnpm install
pnpm dev
```

## Requirements

- **Go 1.25** or later.
- An **Azure Storage Account** (or [Azurite](https://github.com/Azure/Azurite) for local development).

## Installation

```bash
go get github.com/atsika/aznet
```

## Contributing

Contributions are welcome! Please see the existing code structure and driver implementations.

## License

MIT License - see [LICENSE](LICENSE) file for details.

## Credits

- **Noise Protocol**: [noiseprotocol.org](https://noiseprotocol.org/)
- **Azure SDK for Go**: [github.com/Azure/azure-sdk-for-go](https://github.com/Azure/azure-sdk-for-go)

---

**Built with ❤️ by [Atsika](https://x.com/_atsika)**
