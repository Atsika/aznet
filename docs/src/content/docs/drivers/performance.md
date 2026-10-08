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

## Shared-core reads — 2026-10-07

The subsequent [shared-core study](/drivers/core-performance) measures buffered
read latency, allocation work, throughput and SDK-request efficiency across
Blob, Queue and Table. It holds the Table change below constant in both variants.

## Table response echo — 2026-10-07

Direct aznet measurements identified an unused Table insert response as avoidable
work. The baseline is `fb40b8f4abee9bf64357593a744d730617924fa9`; the candidate adds
`Prefer: return-no-content` to `tableTransport.WriteRaw` using the pinned Azure
SDK's per-call header context. Azure acknowledges these inserts with `204` instead
of echoing the entity with `201`. aznet never consumed that echoed entity.
[Azure documents the response preference](https://learn.microsoft.com/en-us/rest/api/storageservices/setting-the-prefer-header-to-manage-response-echo-on-insert-operations).

The original 8 MiB, 64 KiB-write Table profile allocated approximately 265 MiB
process-wide, including setup and profiling overhead. `AddEntity` accounted for
about 91 MiB cumulatively, with its response handler decoding the echoed entity
and the public SDK marshaling it again. The candidate removes this response path;
it does not replace the SDK, change the wire entity, batch writes, or defer commit
acknowledgement. Sequence receipts, ciphertext retries, reclamation, polling and
buffer limits retain their existing behavior. The core and other drivers are
unchanged.

Both endpoints ran the same revision inside the same native Go process on an
Apple M2 Pro (10 CPUs, 16 GiB), macOS 26.5.2 / Darwin 25.5.0, Go 1.26.5 darwin/arm64.
The local emulator was Azurite 3.34.0, Docker image
`sha256:e0ec71c5496f2f1dd14efd5880152b0501e18d6092375347835630e67ccbe044`,
in a dedicated disposable container, with a 10-CPU/8-GiB Docker VM. No other
workload was deliberately run concurrently with the performance comparisons.

The [existing workload harness](/reference/metrics#additional-workloads--2026-10-07)
was extended with concurrent one-way/duplex traffic and delivery after silence.
Every stream checks bytes, block order and receiver EOF. Useful bytes are counted
once, at the receiver. Duplex throughput sums both directions. Poll settings are
1 ms fast/accept and 10 ms maximum data poll, ping disabled, with default buffer
limits (8 MiB pending/decrypted, 4 MiB write/retry). These are measurement
settings, not a default-tuning recommendation.
Setup/cleanup are excluded from workload rates but retained in lifecycle totals.

Full measurements are retained as CSV: [workloads](/measurements/aznet-20261007-workloads.csv),
[SDK attempts by operation/status](/measurements/aznet-20261007-requests.csv),
[setup and lifecycle](/measurements/aznet-20261007-lifecycle.csv), and
[process CPU time](/measurements/aznet-20261007-cpu.csv).
Each row identifies its experiment, variant, source run and case. CPU totals
include setup/cleanup and the harness; memory samples are process-wide Go heap,
not Azure/emulator memory. Sampled peaks can miss brief allocations. Profiles and
the original full logs remain under `.scratch/performance-20261007/` in the working
checkout; the CSVs retain the measurement values independently of that ignored
directory.

### Local paired results

Three alternating pairs used 4 MiB per direction/session and exact 64 KiB writes.
Entries are medians of per-run values, not pooled percentile estimates.

| Sessions | Direction | MiB/s before → after | Write p95 ms before → after | SDK attempts/MiB before → after | Allocated MiB before → after |
| ---: | :--- | ---: | ---: | ---: | ---: |
| 1 | One-way | 15.73 → 14.99 | 5.45 → 5.81 | 24.50 → 21.25 | 132.68 → 108.03 |
| 1 | Duplex | 29.75 → 34.26 | 5.63 → 4.69 | 25.12 → 21.00 | 267.96 → 215.75 |
| 4 | One-way | 31.78 → 45.31 | 11.89 → 9.24 | 29.69 → 24.38 | 515.03 → 421.49 |
| 4 | Duplex | 31.66 → 46.82 | 25.35 → 17.51 | 31.91 → 29.69 | 990.88 → 796.29 |
| 16 | One-way | 31.03 → 44.46 | 55.63 → 37.04 | 31.62 → 31.45 | 1897.52 → 1512.97 |
| 16 | Duplex | 29.28 → 45.01 | 89.65 → 55.15 | 32.23 → 30.56 | 3731.08 → 2977.64 |

Single-session forward throughput did not improve. At four sessions, forward
throughput improved 43% and duplex 48%. Sixteen-session rates improved 43% and
54%. Allocations fell approximately 18–20%, but **sampled peak heap did not fall**:
for example, four-session forward median peak heap rose from 12.46 to 13.50 MiB.
Reduced allocation volume is not a promise of lower resident memory; buffer limits
are unchanged.

A separate three-pair run used 64 MiB/session with 256 KiB writes and crossed
reclamation thresholds. Four-session throughput increased from 38.01 to 54.33
MiB/s; write p95 fell from 40.40 to 27.79 ms; allocations fell 18.5%; attempts/MiB
fell from 7.10 to 5.76. Single-session throughput was 19.52 versus 19.20 MiB/s.
For the identical two-case process (one and four sessions), median user+system
CPU time fell from 19.78 to 16.55 seconds. This includes the measurement harness.

Small-message latency varied with host scheduling. A follow-up of five alternating
pairs, each containing ten 16-exchange tests, produced 800 roundtrips per variant.
Median per-run p50 was 2.22 → 2.05 ms and p95 was 3.08 → 2.81 ms. The initial
mixed-workload runs were noisier; no universal latency improvement is claimed.

### Write-size and driver baselines

Single-session local forward sweep, 4 MiB delivered per case, one run per point.
Each cell gives **MiB/s; SDK attempts per delivered MiB**. Queue/Table chunking can
split an application write. Small writes materially increase request intensity;
these runs predate write batching (see `WithBufferLimits` in the options reference).

| Application write | Blob baseline | Queue baseline | Table baseline | Table candidate |
| ---: | ---: | ---: | ---: | ---: |
| 1 KiB | 0.47; 3019.25 | 0.60; 3762.75 | 1.83; 2027.25 | 2.17; 2014.00 |
| 16 KiB | 7.71; 193.75 | 6.05; 252.50 | 12.80; 105.50 | 15.36; 82.50 |
| 64 KiB | 18.44; 61.50 | 9.60; 138.00 | 17.27; 25.25 | 18.07; 21.25 |
| 256 KiB | 52.07; 14.25 | 11.62; 106.25 | 17.48; 7.25 | 18.83; 6.50 |
| 1024 KiB | 71.94; 3.25 | 11.99; 97.50 | 17.36; 4.75 | 17.54; 4.25 |

The CSVs also retain the original idle/interactive/bulk/slow workloads for all
three drivers at 1/4/16 sessions, and concurrent 8 MiB forward baselines at those
session counts. Original bulk throughput should not be compared directly with
concurrent streams. The one-second idle measurements and first-delivery-after-
silence measurements show no consistent improvement; polling was not changed.

### Live Azure paired results

Both native endpoints ran on the same Mac and used the user-selected accounts:
one for Blob and a separate shared account for Queue/Table. The data-plane
account-info API confirmed `BlockBlobStorage` / `Premium_LRS` for Blob and
`StorageV2` / `Standard_LRS` for Queue/Table. Storage region was not independently
verified: the management login had expired, while account-key data access worked.
Account identifiers and credentials
are kept out of these published artifacts. The two accounts' absolute driver
numbers are not a controlled service comparison.

Five alternating transfer pairs used **16 MiB per direction/session, 256 KiB
writes**, and the same settings/account for both versions. Three pairs also
measured interactive traffic and delivery after one second of silence. These
are finite client-to-Azure tests, not same-region VM benchmarks or capacity SLOs.

| Sessions | Direction | MiB/s before → after | Write p95 ms before → after | SDK attempts/MiB before → after | Allocated MiB before → after |
| ---: | :--- | ---: | ---: | ---: | ---: |
| 1 | One-way | 2.51 → 3.98 | 159.66 → 121.11 | 11.12 → 6.94 | 506.01 → 426.44 |
| 1 | Duplex | 4.57 → 6.99 | 175.33 → 140.26 | 11.88 → 8.16 | 1014.88 → 805.14 |
| 4 | One-way | 8.12 → 12.33 | 193.52 → 173.91 | 11.95 → 7.77 | 1929.52 → 1556.14 |
| 4 | Duplex | 11.45 → 14.77 | 280.65 → 289.24 | 11.42 → 10.49 | 3757.18 → 3047.64 |

Forward throughput improved **58% at one session and 52% at four sessions**;
duplex improved 53% and 29%. Forward SDK attempts/MiB fell 38% and 35% respectively.
Insert count is unchanged (65 per direction, including FIN); the reduction comes
from fewer query attempts during these particular transfers. Request counters
remain **SDK attempts, not billing units**. The direct response-efficiency change
is elimination of the unused successful insert response body, verified by `204`
responses for session writes. Bootstrap inserts still return `201`.

One candidate round had much worse network/service latency. Single-session
forward ranges were 2.17–2.70 MiB/s before and 2.47–4.04 after; four-session ranges
were 5.65–9.59 and 5.13–12.80. Keep those ranges alongside the medians. Four-session
duplex write p95 increased slightly (280.65 → 289.24 ms), despite higher throughput.
The optimization is not a claim that every latency percentile improves.

Live 64-byte roundtrip p50 stayed near 110 ms at one session. Per-run p95 medians
were 144 → 202 ms, driven by isolated outliers in the short 16-exchange samples;
four-session p95 was 140 → 138 ms. Wake delivery p95 medians were 55 → 43 ms at
one session and 57 → 66 ms at four. These small samples do not establish a
small-message or post-silence latency benefit.

A longer before/after duplex pair transferred 64 MiB in each direction at one
session. It crossed the 100-row reclamation threshold on both shared-key and SAS
receivers, completed four successful cleanup batches per version, checked all
bytes and EOF, and improved aggregate throughput from 5.17 to 8.19 MiB/s. This
was one correctness/boundary pair, not an additional five-round estimate.

The live workload runs delivered 2.734 GiB of application data and recorded 39,004
lifecycle SDK attempts, including setup and cleanup. Independent catalog reads
are additional. The actual Azure charge was not queried. Pre-existing catalogs
were unchanged afterward (5 Blob containers, 18 queues, 14 tables), with no new
resources remaining; no pre-existing resource was deleted.

### Correctness and reproducibility

As a separate host TCP control, iperf3 3.21 ran on IPv4 loopback for 3 measured
seconds after 1 omitted second: forward 6.53 Gbit/s, reverse 6.77 Gbit/s,
simultaneous duplex 3.19 + 3.19 Gbit/s, and four parallel forward streams
6.16 Gbit/s aggregate. [Receiver results](/measurements/aznet-20261007-iperf.csv)
are retained. Commands used `iperf3 -s -1 -B 127.0.0.1 -p PORT -J` and
`iperf3 -c 127.0.0.1 -p PORT -t 3 -O 1 -J`, with `-R`, `--bidir` or `-P 4`.
This is a loopback control, not aznet throughput or a measurement of the route to
Azure. Exact aznet write sizes and useful delivered bytes come from the Go harness.

The real-SDK regression test first failed on the baseline's missing response
preference, then passed on the candidate. It verifies bodyless success, commit
followed by response loss, an unchanged retry entity, `EntityAlreadyExists`
reconciliation, and suppression of already-confirmed retransmission. Azurite
reclamation tests assert actual bodyless `204` replies for both credential paths.
The existing tests cover cancellation, bounded buffers, byte identity, ordering,
retry ciphertext and cleanup; the streaming harness additionally verifies FIN
and receiver completion.

`AZNET_AZURITE=1 AZNET_MEASURE=1 go test -race -count=1 -timeout=12m ./...`
passed, including all 63 opt-in measurement cases. `go vet ./...`, native
`go build ./...`, and `GOOS=js GOARCH=wasm go build ./...` also passed. The race-run
timings are excluded from the performance CSVs.

The runtime candidate is identified by `aztable.go` SHA-256
`0a2c9038cded83a13d2145d64b75e519fc904e0e1e8c5b032258f8384512015e`.
Both binaries in each comparison used an identical copy of the current harness.
No race instrumentation was used for performance numbers. GC/memory/processor
runtime environment overrides were unset. The comparison script retains the
candidate diff and binary/harness checksums.

Reproduce local paired measurements with `tests/performance/compare.sh`. For the
long forward run, set `AZNET_MEASURE_WRITE_SIZE=262144`,
`AZNET_MEASURE_STREAM_BYTES=67108864` and
`AZNET_MEASURE_FILTER='^TestSDKWorkloadMeasurement$/aztable/connections(1|4)$/stream$'`.
For the local sweep, use 1024/16384/65536/262144/1048576-byte writes, 4 MiB per case,
and the one-session stream filter across all drivers. For live comparisons, use
the [explicit account configuration](/reference/metrics#explicit-live-measurements)
and 16 MiB/session; run two additional transfer-only pairs after the script's
three pairs to reproduce the five-round design. Longer Azure duplex validates
SAS reclamation, which Azurite 3.34 cannot emulate correctly.

## Reproduce before tuning

Record both endpoint revisions and the actual published module or explicit workspace used. Hold payload, concurrency, polling, limits, region and account constant; alternate old/new runs and repeat. Measure application bytes, latency distributions, SDK attempts, allocations and sampled peak memory. Separate race correctness runs from performance timings.

Keep byte identity, ordered EOF, cancellation, bounded memory and cleanup as correctness gates. Faster results that lose bytes or increase resource retention are not acceptable improvements. See [validation and migration](/guides/validation) for what ran and what remains deployment-specific.
