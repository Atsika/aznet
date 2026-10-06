---
title: Azure Blob Storage Driver
description: Append blobs, range reads, and independent directional progress.
---

`azblob` uses one session container with separate request and response append blobs. The initiator appends to the request channel and reads the response channel; the listener uses the opposite direction.

## Data path

Writes use conditional append positions so an uncertain retry does not append the same ciphertext twice. The core retains the identical sealed chunk until completion. Reads use ranged downloads; the receive offset advances only by bytes actually read, including bytes returned alongside an error. Fetching headers does not consume the advertised body length.

Independent transmit and receive locks allow appends to progress while a download is waiting, and downloads to progress while an append is waiting. Operations within each direction remain ordered. This removes cross-direction lock contention; it does not remove Azure request latency or polling delay.

## Limits and rotation

The library chooses a **4 MiB sealed-chunk ceiling** (`MaxBlobBlockSize`). This is an aznet policy, not a claim about the largest append supported by every Azure API version. The application MTU is smaller and also respects configured write/retry limits.

The driver signals rotation near 49,990 appended blocks. An ordered rotation control frame transfers both peers to the next blob. Failed creation preserves the current resource identity and offsets for retry.

Blob implements `ReadRawLimit`: range downloads respect the remaining receive allowance and may stop inside an encrypted chunk, whose unread suffix is fetched later. See the [driver contract](/guides/developing-drivers).

## Deployment and authorization

Standard general-purpose v2 and Premium block blob accounts support append blobs; choose using your own [measurements](/drivers/performance). Premium is not required and no fixed speedup is promised.

The session SAS is scoped to its container and permits Read, List, Add, Create and Write. The same token serves both directions; it is not a permission boundary between request and response blobs. Accepted-connection teardown owns session-container deletion. Shared bootstrap containers require separate administrator cleanup.

See [security](/core-concepts/security), [cost](/drivers/cost), and [closing ownership](/reference/api#closing-and-resource-ownership).
