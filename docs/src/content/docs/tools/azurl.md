---
title: azurl
description: Generate aznet client connection strings with embedded SAS tokens.
---

`azurl` is a command-line utility for the `aznet` project that simplifies the generation of client connection URLs.
It handles the creation of scoped SAS tokens and formats the URL correctly for use with `aznet.Dial`.

## Installation

You can install `azurl` using the Go toolchain:

```bash
go install github.com/atsika/aznet/cmd/azurl@latest
```

## Usage

The tool requires the service type, the storage endpoint URL, and your account credentials.

```bash
azurl [-driver <type>] -url <url> -account <account> -key <key> [options]
```

### Flags

| Flag         | Description                                                                                                             |
| :----------- | :---------------------------------------------------------------------------------------------------------------------- |
| `-driver`    | The driver type (`azblob`, `azqueue`, or `aztable`). (default: `azblob`)                                                |
| `-url`       | The service URL (e.g., `https://mystorage.blob.core.windows.net`). (default: `http://localhost:10000/devstoreaccount1`) |
| `-account`   | The Azure Storage account name. (default: `devstoreaccount1`)                                                           |
| `-key`       | The Azure Storage account key. (default: Azurite master key)                                                            |
| `-handshake` | Handshake endpoint name (default: `handshake`).                                                                         |
| `-token`     | Token endpoint name (default: `token`).                                                                                 |
| `-expiry`    | SAS token expiry duration (default: `24h`).                                                                             |
| `-env`       | Use credentials from environment variables (`AZURE_STORAGE_ACCOUNT`, `AZURE_STORAGE_ACCOUNT_KEY`).                      |

### Examples

**Local Development (Azurite):**

```bash
azurl -driver aztable -url http://localhost:10002/devstoreaccount1 -account devstoreaccount1 -key Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==
```

**Azure Storage (Using Environment Variables):**

```bash
export AZURE_STORAGE_ACCOUNT="myaccount"
export AZURE_STORAGE_ACCOUNT_KEY="mykey"
azurl -url https://myaccount.blob.core.windows.net -env
```

**Azure Storage (Using Flags):**

```bash
azurl -url https://account.blob.core.windows.net -account account -key key -expiry 1h
```

## How it works

When you run `azurl`, it performs the following steps:

1. **Authenticates** with your Azure Storage account using the provided credentials (flags, environment variables, or defaults).
2. **Generates** two separate Shared Access Signature (SAS) tokens with minimal permissions:
   - **Handshake SAS**: Grants `Add`, `Create`, and `Write` for Blob; `Add` for Queue and Table.
   - **Token SAS**: Grants `Read` and `List` for Blob; `Read` for Queue and Table.
3. **Encodes** these tokens using **Base64 URL encoding**.
4. **Constructs** a final URL that includes these tokens as query parameters.

## Why use azurl?

The output URL is designed to be a complete, self-contained connection string.
It allows a client to connect to a server without needing the master account key,
using only the limited permissions granted by the embedded SAS tokens.

1. **Security**: Only the necessary permissions are granted.
2. **Portability**: The URL contains everything needed for `aznet.Dial`.
3. **Automation**: Easily generate URLs for automated clients or testing.

## Security Considerations

- **SAS Expiration**: The generated SAS tokens are valid for the duration specified by `-expiry` (default **24 hours**). After they expire, the client will receive `403 Forbidden` errors.
- **Token Encoding**: SAS tokens often contain special characters. `azurl` ensures these are safely Base64 URL-encoded so they don't interfere with the overall URL structure.
- **Credential Safety**: `azurl` is intended to be run by the server administrator. The output URL does *not* contain your master Account Key.
