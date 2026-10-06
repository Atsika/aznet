---
title: Examples
description: Explore working examples of aznet in action.
---

The `aznet` repository includes working examples in the `examples/` directory to help you understand how to use the library in different scenarios.

To run the examples:

1. Start Azurite or have an Azure Storage account ready.
1. Run the server in one terminal:

```bash
go run ./examples/echo/server
```

1. Configure the client with a fresh connection URL from the server. The checked-in URL is illustrative and may be expired. Treat the generated URL as a secret.
1. Run the client in another terminal:

```bash
go run ./examples/echo/client
```

## Echo

A simple demonstration of bi-directional communication. The client sends lines of text to the server, which echoes them back.

- **Location**: `examples/echo/`
- **Driver**: Demonstrates usage with `azblob`, `azqueue`, and `aztable`.

## Metrics Collection

Demonstrates how to use the built-in metrics system to monitor connection health and API usage. The client sends 100MB of random data to the server, which echoes it back. Both client and server print detailed metrics reports after the transfer completes.

- **Location**: `examples/metrics/`
- **Key features**:
  - Uses default metrics implementation (no custom metrics needed)
  - Transfers 100MB of random data
  - Shows all transaction types (Write, Read, List, Delete)
  - Displays data transfer statistics (bytes sent/received)
- **Usage**: Run the server first, then the client. Both will print metrics reports when the transfer completes.

These examples illustrate API usage; the [validation guide](/guides/validation) identifies the automated suites and exact revisions actually exercised. Check write/close errors and use application-level completion when final delivery matters.
