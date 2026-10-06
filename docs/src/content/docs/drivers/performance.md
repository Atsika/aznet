---
title: Performance Measurement
description: Measured evidence and the limits of driver comparisons.
---

Polling and Azure request latency are part of every connection. Throughput also depends on application write sizes, batching, receive consumption, concurrency, service throttling and account placement. A driver's raw chunk ceiling alone does not predict speed.

## Retained measurements

The [SDK measurement baseline](/reference/metrics#reproducing-measurements) uses local Azurite and includes idle, interactive, bulk and concurrent cases. Emulator results validate request accounting and provide a repeatable local comparison; they are not Azure throughput predictions.

On 2026-10-06, a same-account native ProxyBlob workload compared aznet `9e6683d` with the directional Blob-lock change (`aa3fec8`, merged as `7abcd80a7a2820d69e55deb1fc62f8b5a601f4be`). Two alternating rounds each ran 3 BIND conversations, 3 public DNS queries and 45 UDP echo cases, with independent zero-residue checks afterward.

| Round | Before echo median | After echo median |
| :--- | ---: | ---: |
| 1 | 974 ms | 481 ms |
| 2 | 995 ms | 483 ms |

DNS latency did not improve consistently. These are whole ProxyBlob conversation measurements, not direct aznet bulk-transfer benchmarks. The separate instrumented pair found cross-direction mutex waits dropped while mean SDK header/append calls still took roughly 120–230 ms. Polling also remained significant. Do not sum overlapping directional totals or infer a general Azure SLO.

Exact workload settings, host/toolchains, concurrency, heap measurements and the profile are retained in ProxyBlob's [measured-workload report](https://github.com/quarkslab/proxyblob/blob/6493d6329a62dc3a1ff5a2f0ca3823b5926ea46b/docs/performance.md) and its linked baseline. ProxyBlob's per-stream credit budgets are an application layer above aznet's connection buffers.

## Reproduce before tuning

Record both endpoint revisions and the actual published module or explicit workspace used. Hold payload, concurrency, polling, limits, region and account constant; alternate old/new runs and repeat. Measure application bytes, latency distributions, SDK attempts, allocations and sampled peak memory. Separate race correctness runs from performance timings.

Keep byte identity, ordered EOF, cancellation, bounded memory and cleanup as correctness gates. Faster results that lose bytes or increase resource retention are not acceptable improvements. See [validation and migration](/guides/validation) for what ran and what remains deployment-specific.
