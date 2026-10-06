---
title: Local Development with Azurite
description: How to use the Azurite storage emulator for local testing and development.
---

[Azurite](https://github.com/Azure/Azurite) is an open-source Azure Storage API emulator. It's the recommended way to develop and test `aznet` applications locally without incurring Azure costs.

## Running Azurite

The easiest way to run Azurite is via Docker:

```bash
docker run --rm --name aznet-dev \
    -p 127.0.0.1:10000:10000 -p 127.0.0.1:10001:10001 -p 127.0.0.1:10002:10002 \
    mcr.microsoft.com/azure-storage/azurite:3.34.0 \
    azurite --blobHost 0.0.0.0 --queueHost 0.0.0.0 --tableHost 0.0.0.0 --skipApiVersionCheck
```

The retained integration runs used Azurite 3.34.0 with `--skipApiVersionCheck` because the SDK requests a newer API version. This bypass supports emulator testing; it does not establish parity with every live Azure behavior.

## Connecting with aznet

Azurite uses a well-known account name and key for local development:

- **Account**: `devstoreaccount1`
- **Key**: `Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==`

### URL Format

When using Azurite, specify the local endpoint in the address. It's recommended to use environment variables for credentials to keep the URLs clean:

```bash
export AZURE_STORAGE_ACCOUNT=devstoreaccount1
export AZURE_STORAGE_ACCOUNT_KEY=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==
```

```go
// For Blob Storage
address := "http://localhost:10000/devstoreaccount1"
listener, _ := aznet.Listen("azblob", address)

// For Queue Storage
address := "http://localhost:10001/devstoreaccount1"
listener, _ := aznet.Listen("azqueue", address)

// For Table Storage
address := "http://localhost:10002/devstoreaccount1"
listener, _ := aznet.Listen("aztable", address)
```

:::tip
If you prefer embedding credentials in the URL, ensure the Storage Key is **URL-encoded** (e.g., replace `/` with `%2F`).
:::

## Troubleshooting Azurite

### 1. Version Compatibility

Ensure you are using a recent version of Azurite. `aznet` relies on features (like Append Blobs) that might not be fully implemented in very old versions.

### 2. HTTPS vs HTTP

Azurite runs over HTTP in this example. Specify `http://` explicitly; use HTTPS for live Azure.

### 3. Cleaning Up

If tests crash, owned resources may remain. Stop and remove the disposable container above to discard its unmounted storage. Restarting a container or recreating one with a persistent volume does not erase that volume. Never clear a shared emulator’s data as a substitute for namespace cleanup.

## Run the SDK integration suite

With the emulator running at the default ports above:

```bash
AZNET_AZURITE=1 GOWORK=off go test -mod=readonly -race -count=1 ./...
```

Without the opt-in environment variable, emulator tests skip. See [metrics](/reference/metrics) for the measurement-only invocation and [validation](/guides/validation) for the separate live Azure evidence.
