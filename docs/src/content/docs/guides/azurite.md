---
title: Local Development with Azurite
description: How to use the Azurite storage emulator for local testing and development.
---

[Azurite](https://github.com/Azure/Azurite) is an open-source Azure Storage API emulator. It's the recommended way to develop and test `aznet` applications locally without incurring Azure costs.

## Running Azurite

The easiest way to run Azurite is via Docker:

```bash
docker run -p 10000:10000 -p 10001:10001 -p 10002:10002 \
    mcr.microsoft.com/azure-storage/azurite
```

Alternatively, use the provided `docker-compose.yml` in the root of the `aznet` repository:

```bash
docker-compose up -d
```

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

By default, Azurite runs over HTTP. `aznet` handles this automatically if you specify `http://` or `localhost` in the endpoint URL.

### 3. Cleaning Up

If your tests crash, Azurite might still have old containers or queues. You can reset Azurite by deleting its data directory (usually `__blobstorage__`, `__queuestorage__`, etc.) or by restarting the container.
