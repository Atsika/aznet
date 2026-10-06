---
title: Driver Overview
description: Compare transport mechanisms and deployment requirements.
---

All three drivers implement the same `Factory` → `Driver` → `Transport` contracts. Select a driver using measurements of your workload and account, rather than a universal speed or price ranking.

| Driver | Storage mechanism | Raw chunk ceiling chosen by aznet | Main considerations |
| :--- | :--- | :--- | :--- |
| [Blob](/drivers/azblob) | Append blobs and range downloads | 4 MiB | Independent read/write locks; rotation; consumed-byte offsets |
| [Queue](/drivers/azqueue) | Base64 messages with sequence headers | 49,080 bytes | Reassembly, duplicate suppression, message-size overhead |
| [Table](/drivers/aztable) | Sequential entities with binary properties | 960 KiB | Bounded prefetch and consumed-row reclamation |

These are sealed-chunk ceilings, not application MTUs or connection memory budgets. Encryption, framing and configured write/retry limits further constrain a frame. See [buffer limits](/reference/options#withbufferlimits).

Standard general-purpose v2 accounts support all three services. Premium block blob accounts support block and append blobs, not Queue or Table. Account availability is distinct from a measured performance advantage. See Microsoft's [account types](https://learn.microsoft.com/en-us/azure/storage/common/storage-account-overview#types-of-storage-accounts).

All drivers incur polling, setup, retry and cleanup requests as well as data requests. Compare [measured performance](/drivers/performance) and [cost methodology](/drivers/cost) under the same concurrency, polling settings, region, account tier and payload mix.
