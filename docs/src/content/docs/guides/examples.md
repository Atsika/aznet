---
title: Examples
description: Explore working examples of aznet in action.
---

Start with [Getting started](/getting-started). Its `examples/quickstart` program runs one complete request/response with byte verification, ordered EOF, application acknowledgement and explicit demo cleanup. It accepts `-driver`, `-listen`, `-namespace` and the client `AZNET_URL` environment variable; no source edits or checked-in SAS token are needed.

## Interactive echo example

The older `examples/echo` program is useful for manual interactive exploration. Its driver and endpoint are package variables: configure both sides, start the server, then replace the client's illustrative URL with the fresh one the server prints. Run `go run ./examples/echo/server` and `go run ./examples/echo/client` from the repository root. Its stdin EOF exits the client without a delivery acknowledgement, so use the quickstart example for a complete, verified one-shot exchange.

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
