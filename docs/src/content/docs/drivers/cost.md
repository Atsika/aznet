---
title: Cost Measurement
description: Estimate cost from observed requests and current account pricing.
---

There is no fixed cost ratio between the drivers. Polling, batching, retries, session lifetime and cleanup change request counts even when the delivered application bytes are identical.

## Collect comparable evidence

1. Record aznet/application revisions, toolchain, region, account kind, redundancy, polling and buffer settings.
2. Run idle, interactive, bulk and concurrent workloads for a stated duration. Include setup, empty polling, retries, rotation and cleanup.
3. Capture [SDK request attempts and operation labels](/reference/metrics), delivered application bytes, stored bytes over time and network transfer.
4. Apply the current prices for that service, region, tier and operation category. Reconcile with Azure billing before treating the estimate as a budget.

Metrics are **not billing units**. A Table batch is one observed outer SDK request but its billed treatment need not match that counter. Retried attempts and empty polls count too. Transport byte counters include ciphertext and framing, but are neither the application-byte total nor complete billed HTTP traffic.

Use official [Blob](https://azure.microsoft.com/en-us/pricing/details/storage/blobs/), [Queue](https://azure.microsoft.com/en-us/pricing/details/storage/queues/), and [Table](https://azure.microsoft.com/en-us/pricing/details/storage/tables/) pricing for the target deployment. No historical unit prices or annual extrapolation are presented as current quotes.

## Retention and cleanup

Session storage remains until it is consumed/reclaimed or the accepted connection cleans it up. Table keeps a bounded consumed-row tail as an uncertain-write receipt; unread data may remain much larger. Blob retains appended data until rotation/session resource deletion. Memory limits do not cap all remote storage.

Bootstrap resources deliberately outlive listener Close. Crashes, unavailable storage, or failed cleanup can leave resources behind. Inspect cleanup errors and independently verify the owned namespace; a timeout is not proof of deletion.
