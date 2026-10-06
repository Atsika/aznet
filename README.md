# aznet

<p align="center"><img src="./docs/src/assets/aznet.png" width="300"></p>

Go `net.Conn` and `net.Listener` interfaces over Azure Blob, Queue and Table Storage.

aznet provides an ordered encrypted byte stream using storage requests and polling. Applications use familiar Go I/O, but must account for storage latency, credential lifetime, finite buffers and cleanup ownership. Noise NN encrypts application frames without authenticating peer identity.

## Start here

```bash
go get github.com/atsika/aznet
```

Use Go with automatic toolchain selection enabled: this revision declares Go 1.25 and selects Go 1.26.5. An Azure Storage account or local Azurite is required for integration use. Standard general-purpose v2 supports all three drivers; Premium Blob is not required.

- [Getting started](docs/src/content/docs/getting-started.md)
- [API, delivery and cleanup contracts](docs/src/content/docs/reference/api.md)
- [Custom driver contracts and migration](docs/src/content/docs/guides/developing-drivers.md)
- [Validation evidence and rollout requirements](docs/src/content/docs/guides/validation.md)
- [Security and authorization](docs/src/content/docs/core-concepts/security.md)
- [Measured performance](docs/src/content/docs/drivers/performance.md)

Full Close may delete unread session data. For delivery-sensitive responses, use CloseWrite and application-level completion before Close. Listener Close retains shared bootstrap resources; their administrator removes them explicitly after all namespace users stop. Neither credential renewal nor crash-proof remote cleanup is automatic.

## Development

```bash
GOWORK=off go test -mod=readonly -race -count=1 ./...
GOWORK=off go vet -mod=readonly ./...
```

External-service tests require explicit opt-in; see the validation guide. To build the Starlight documentation:

```bash
cd docs
pnpm install --frozen-lockfile
pnpm build
```

The Driver/Transport registration mechanism remains the extension point. See the driver guide before changing ordering, retries or resource ownership.

## License and credits

[MIT](LICENSE). Built with the [Noise Protocol](https://noiseprotocol.org/) and [Azure SDK for Go](https://github.com/Azure/azure-sdk-for-go).
