---
title: Azure Queue Storage Driver
description: Sequenced messages and bounded queue reassembly.
---

`azqueue` creates request and response queues per session. Each message contains an eight-byte sequence number followed by the sealed chunk, encoded as Base64.

## Size and ordering

`MaxRawSize()` is `(64 KiB × 3 / 4) - 8 - 64`, or **49,080 bytes**. The eight bytes hold the sequence and the additional 64 bytes provide a safety margin. Framing and encryption further reduce the application MTU.

Azure queue delivery is not an ordered byte stream. The driver reassembles by sequence, suppresses duplicate chunks, bounds pending reassembly and detects a stalled missing sequence. A malformed message or permanent ordering failure is observable; it must not be mistaken for an empty poll. Message deletion is part of queue processing, not an application-level delivery acknowledgement.

## Bootstrap and session resources

Handshake and token queues are shared. Token lookup peeks at up to 32 messages; handshake responses have a TTL derived from the listener's connect timeout, rounded to whole seconds. Expiry bounds stale-response obstruction; it does not guarantee that an arbitrary burst beyond the peek window will connect successfully.

Session SAS permissions are Add for the client's request queue and Read/Process for its response queue. The accepted side owns session cleanup. Listener Close leaves shared bootstrap queues in place; a namespace administrator removes them explicitly after all users stop.

## Choosing Queue

Use a Standard general-purpose v2 account. Premium block blob accounts do not provide Queue storage. Small messages, Base64 expansion, polling and delete requests affect both latency and request count. There is no unconditional cheapest-driver ranking: measure [SDK attempts](/reference/metrics), [performance](/drivers/performance), and [cost](/drivers/cost) for the intended workload.
