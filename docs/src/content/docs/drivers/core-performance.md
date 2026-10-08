---
title: Shared-core read performance
description: Before and after measurements of buffered reads and plaintext ownership across all three storage drivers.
---

## Scope and mechanism

This second 2026-10-07 comparison isolates **shared connection changes**. Both
versions include the [earlier Table response improvement](/drivers/performance#table-response-echo--2026-10-07).
The baseline is `fb40b8f4abee9bf64357593a744d730617924fa9` plus this exact
[Table-only patch](/measurements/aznet-core-20261007-baseline.patch).

Profiling the encrypted in-memory transfer benchmark identified repeated
operation-context creation as the main allocation source when applications read
in small pieces. A 64 KiB transfer read in 64-byte pieces allocated 4,119 times.
Each buffered Read created cancellation/deadline bookkeeping even though the
plaintext was already authenticated and available. The new fast path checks
connection state, cancellation and the current deadline while retaining the read
gate and buffer lock; it then copies available frame payload directly to the
caller. Blocking/contended operations retain the interruptible context path.

A second change transfers ownership of the decrypted buffer when the prior
plaintext buffer is empty. Its fully consumed storage becomes the next decrypt
scratch buffer. Incomplete frame prefixes still use the append path. This
removes one full plaintext copy without aliasing unread plaintext. Both changes
live in the driver-agnostic connection; framing, wire format, ciphertext retries,
commit acknowledgement, polling, buffer limits and driver interfaces are unchanged.

## Method

Both endpoints run in the same native process on the host used for the earlier
study: Apple M2 Pro, 10 CPUs, 16 GiB; macOS 26.5.2 / Darwin 25.5; Go 1.26.5
arm64. Local storage is dedicated Azurite 3.34.0. Live Blob uses `premiumblob`
(BlockBlobStorage / Premium_LRS); Queue and Table use `standardacct`
(StorageV2 / Standard_LRS). Account names are anonymized throughout these measurements. Account regions remain unverified because the local
management-plane login is expired. These are client-to-service observations,
not same-region capacity estimates or fair cross-tier driver rankings.

The existing SDK workload harness is identical in both binaries. Three paired
rounds alternate before/after, after/before, before/after. Stream/duplex use
64 KiB writes, 4 MiB per direction/session, 1 or 4 sessions, and 64-byte reads.
Local forward streams also sweep 1 KiB and 64 KiB reads. Polling is 1 ms fast/
accept and 10 ms maximum data interval, ping disabled, default buffer limits.
Every stream verifies bytes, order and FIN/EOF. Useful throughput counts each
received byte once; duplex sums both directions. Interactive request/reply,
one-second idle, and one-byte delivery after that silence are also measured.

Tables contain medians of three per-run values, not pooled percentiles. The
separate Read timings sample at most 8,192 calls per direction/session.
With 64-byte reads, these percentiles mostly measure buffered drains; systematic
sampling can under- or overrepresent fetches when its interval aligns with frame
boundaries. These are percentiles of timed samples, not an unbiased estimate of
the full Read-call distribution. **Read-call p95 is not network RTT or message
latency.** Throughput includes receiver completion. Allocation counts/bytes and
sampled heap are process-wide, including SDK and harness activity. Sampling is
identical between versions. Setup/cleanup are excluded from workload rates and
retained separately. Three pairs cannot establish a production SLO.

## All-driver results

Before → after; 64-byte application reads. SDK attempts count both endpoints
and are not Azure billing units.

### Local Azurite

| Driver | Sessions | Throughput MiB/s | Read p95 ns | Allocations | Allocated MiB | SDK attempts/MiB |
| :--- | ---: | ---: | ---: | ---: | ---: | ---: |
| azblob | 1 | 13.19 → 15.32 | 1,250 → 333 | 304,770 → 55,719 | 21.88 → 6.13 | 41.00 → 53.25 |
| azblob | 4 | 51.31 → 51.02 | 750 → 208 | 1,176,870 → 140,562 | 85.40 → 20.78 | 31.44 → 34.19 |
| azqueue | 1 | 8.11 → 6.67 | 917 → 209 | 372,968 → 124,805 | 111.72 → 96.09 | 116.50 → 138.25 |
| azqueue | 4 | 16.67 → 16.07 | 542 → 125 | 1,622,846 → 603,130 | 451.77 → 388.87 | 168.31 → 179.62 |
| aztable | 1 | 14.00 → 14.72 | 709 → 125 | 293,449 → 31,479 | 124.63 → 108.00 | 21.25 → 21.25 |
| aztable | 4 | 44.71 → 46.55 | 584 → 84 | 1,179,239 → 135,847 | 487.45 → 415.65 | 23.12 → 24.31 |

Duplex includes both directions:

| Driver | Sessions | Throughput MiB/s | Read p95 ns | SDK attempts/MiB | Sampled peak heap MiB |
| :--- | ---: | ---: | ---: | ---: | ---: |
| azblob | 1 | 33.09 → 38.99 | 792 → 209 | 39.38 → 43.50 | 4.15 → 3.77 |
| azblob | 4 | 66.36 → 70.02 | 625 → 208 | 23.81 → 26.03 | 14.48 → 13.22 |
| azqueue | 1 | 17.13 → 16.55 | 541 → 84 | 126.38 → 132.88 | 4.46 → 4.26 |
| azqueue | 4 | 13.45 → 13.15 | 541 → 84 | 285.38 → 291.91 | 12.30 → 12.14 |
| aztable | 1 | 30.44 → 33.66 | 542 → 84 | 21.25 → 21.38 | 9.45 → 9.02 |
| aztable | 4 | 41.60 → 44.84 | 625 → 125 | 29.34 → 28.97 | 22.77 → 23.94 |

### Live Azure

| Driver | Sessions | Throughput MiB/s | Read p95 ns | Allocations | Allocated MiB | SDK attempts/MiB |
| :--- | ---: | ---: | ---: | ---: | ---: | ---: |
| azblob | 1 | 0.43 → 0.41 | 2,334 → 417 | 296,558 → 34,628 | 22.09 → 5.45 | 33.00 → 33.50 |
| azblob | 4 | 1.65 → 1.62 | 2,333 → 458 | 1,190,830 → 142,923 | 86.63 → 20.92 | 33.06 → 33.44 |
| azqueue | 1 | 0.82 → 0.84 | 2,042 → 541 | 347,923 → 85,426 | 109.44 → 91.91 | 77.00 → 77.50 |
| azqueue | 4 | 3.12 → 3.06 | 1,833 → 458 | 1,413,563 → 354,720 | 436.75 → 367.70 | 76.75 → 77.44 |
| aztable | 1 | 1.42 → 1.39 | 1,208 → 209 | 299,559 → 37,894 | 114.41 → 96.00 | 27.50 → 28.00 |
| aztable | 4 | 5.54 → 5.28 | 792 → 167 | 1,204,662 → 158,955 | 459.58 → 392.34 | 27.06 → 27.88 |

Duplex includes both directions:

| Driver | Sessions | Throughput MiB/s | Read p95 ns | SDK attempts/MiB | Sampled peak heap MiB |
| :--- | ---: | ---: | ---: | ---: | ---: |
| azblob | 1 | 0.83 → 0.85 | 2,292 → 459 | 33.62 → 33.75 | 6.31 → 5.76 |
| azblob | 4 | 3.09 → 3.15 | 2,208 → 458 | 32.75 → 33.56 | 19.19 → 16.69 |
| azqueue | 1 | 1.55 → 1.63 | 1,958 → 500 | 77.50 → 77.88 | 9.85 → 9.86 |
| azqueue | 4 | 4.56 → 4.49 | 1,334 → 209 | 79.41 → 78.22 | 25.67 → 27.50 |
| aztable | 1 | 2.94 → 2.80 | 917 → 167 | 27.00 → 27.50 | 12.78 → 13.07 |
| aztable | 4 | 7.64 → 8.54 | 709 → 125 | 31.62 → 29.06 | 31.74 → 33.08 |

### Longer local forward streams

32 MiB per session, otherwise the same forward-stream settings. Three alternating pairs.

The larger sampling interval can align with frame boundaries: Blob Read p95
includes storage fetches here. Do not compare these sampled tails with the 4 MiB
buffered-read tails or interpret them as isolated CPU latency.

| Driver | Sessions | Throughput MiB/s | Read p95 ns | Allocations | Allocated MiB | SDK attempts/MiB |
| :--- | ---: | ---: | ---: | ---: | ---: | ---: |
| azblob | 1 | 22.01 → 23.52 | 1,471,041 → 1,599,333 | 2,436,970 → 411,323 | 168.97 → 41.11 | 40.75 → 49.09 |
| azblob | 4 | 52.69 → 57.09 | 2,503,750 → 3,263,791 | 9,359,215 → 1,144,796 | 651.63 → 136.38 | 29.64 → 34.52 |
| azqueue | 1 | 10.42 → 9.96 | 709 → 209 | 3,014,657 → 942,776 | 891.51 → 760.67 | 122.22 → 128.19 |
| azqueue | 4 | 16.97 → 16.51 | 542 → 125 | 13,064,176 → 4,850,324 | 3,600.00 → 3,087.86 | 172.94 → 181.20 |
| aztable | 1 | 15.59 → 16.93 | 11,583 → 10,417 | 2,394,771 → 299,240 | 986.26 → 850.65 | 20.31 → 20.31 |
| aztable | 4 | 45.33 → 45.62 | 11,875 → 11,042 | 9,620,959 → 1,280,676 | 3,884.76 → 3,308.75 | 22.10 → 23.52 |

## Isolated shared-core benchmark

Five alternating pairs without profiling, using a bounded in-memory transport,
real encryption/framing and 64 KiB per transfer. Handshake/setup are outside the
timed loop. These are CPU-path results, not cloud throughput. Diagnostic profiles
were collected separately and excluded from this comparison.

| Read bytes | Transfer time µs | Allocated bytes/transfer | Allocations/transfer |
| ---: | ---: | ---: | ---: |
| 1 | 18,931.11 → 3,311.41 | 16,790,219 → 3,308 | 262,164 → 27 |
| 64 | 323.22 → 78.38 | 263,581 → 1,538 | 4,119 → 27 |
| 1,024 | 45.76 → 29.72 | 17,652 → 1,512 | 279 → 27 |
| 32,768 | 28.65 → 27.22 | 1,769 → 1,510 | 31 → 27 |
| 65,536 | 28.39 → 27.12 | 1,513 → 1,510 | 27 → 27 |

[Individual benchmark runs](/measurements/aznet-core-20261007-micro.csv).

## Interpretation and limits

One baseline case (`live-read64-round3-before`, Blob, four-session wake) failed
at the 90-second overall deadline. Two AppendBlock attempts had no HTTP status;
1,226 empty range polls were recorded. Error text was redacted, so this does not
establish the underlying network/service failure. All stream and duplex cases
in that run passed and remain included. The failed wake has no fabricated latency
value; it is retained in the [case outcomes](/measurements/aznet-core-20261007-cases.csv)
and lifecycle/request records. Separate focused wake repeats are labeled
`diagnostic` and do not replace the failure or enter the main three-pair medians.
All six focused repeat cases (three before/after pairs) passed. The accounts'
resource catalogs were unchanged immediately after the failure and again after
all live work: the same 5 containers, 18 queues and 14 tables, with no test
resources remaining. This did not reproduce or explain the original failure.

The isolated 64-byte-read transfer is 4.12× faster with 99.3% fewer allocations;
1 KiB reads are 1.54× faster, while 64 KiB reads reduce transfer time by 4.5%.
These CPU-path gains do not translate directly to Azure throughput.

The demonstrated benefit is less shared-core CPU and allocation work for partial
application reads across all drivers. This does not remove a storage round trip
or change polling policy. End-to-end throughput and request counts remain
sensitive to service latency, batching, poll timing and scheduling. Their mixed
movements are retained rather than treated as a universal throughput or
storage-request saving. Small request/reply messages do not normally exercise
the partial-frame fast path. Idle and wake results are controls, not targeted gains.
Reducing cumulative allocation also does not guarantee a smaller sampled peak
heap; raw peak values are retained.

The earlier loopback iperf3 control measured 6.53 Gbit/s forward and 6.77 Gbit/s
reverse. It characterizes the host TCP path separately; it neither traverses
aznet nor predicts Azure storage throughput. See the
[retained iperf measurements](/measurements/aznet-20261007-iperf.csv).

## Validation and resource usage

The full `AZNET_AZURITE=1 AZNET_MEASURE=1 AZNET_MEASURE_READ_SIZE=64`
`go test -race -count=1 -timeout=12m ./...` run passed, including all 63 workload
cases at 1/4/16 sessions. `go vet ./...`, native and js/wasm builds, and the docs
build passed. Regression tests cover zero-allocation buffered reads, deadline
expiry/reset, cancellation, closure, terminal overflow, gate contention, split
frames, plaintext ownership, order and EOF. Existing retry/deadline/limit tests
also passed; the writer and ciphertext retry state machine were unchanged.

This shared-core study completed 288 local comparison cases and 185 successful
live cases, with the one retained baseline wake failure above. Live main runs
and focused repeats delivered approximately 1.055 GiB and recorded 70,892 SDK
attempts including setup/cleanup. These are usage observations, not a billed-cost
estimate. No provisioned compute was added. Local catalogs were empty after
validation; live catalogs matched their pre-test sets. The dedicated emulator
was removed afterward.

## Evidence and reproduction

For an isolated CPU run:

```sh
go test -run '^$' -bench '^BenchmarkConnTransfer$' -benchmem -benchtime=1s -count=5 .
```

 To compare revisions, build two test
executables with the same `conn_benchmark_test.go` and the `reviewNoise(testing.TB)`
helper from `conn_test.go`, then alternate their invocation with these same test
flags. Do not include profiling in timed comparisons. The preserved five-pair
microbenchmarks use exactly that harness on both sides.

Download [workload values](/measurements/aznet-core-20261007-workloads.csv),
[SDK attempts by operation/status](/measurements/aznet-core-20261007-requests.csv),
[lifecycle values](/measurements/aznet-core-20261007-lifecycle.csv), and
[process CPU times](/measurements/aznet-core-20261007-cpu.csv), and
[case outcomes including failures](/measurements/aznet-core-20261007-cases.csv).
Source logs, exact binaries, source snapshots and diagnostic profiles are retained
under `.scratch/core-performance-20261007/` in this checkout. The CSVs retain
measurement values independently of that ignored directory.

Start a dedicated Azurite as described in the [harness reference](/reference/metrics#reproducing-measurements), then run:

```sh
AZNET_MEASURE_BASELINE_PATCH="$PWD/docs/public/measurements/aznet-core-20261007-baseline.patch" \
AZNET_MEASURE_READ_SIZE=64 AZNET_MEASURE_WRITE_SIZE=65536 \
AZNET_MEASURE_STREAM_BYTES=4194304 AZNET_MEASURE_IDLE_MS=1000 \
AZNET_MEASURE_FILTER='^TestSDKWorkloadMeasurement$/az(blob|queue|table)/connections(1|4)$/(idle|interactive|stream|duplex|wake)$' \
  tests/performance/compare.sh fb40b8f4abee9bf64357593a744d730617924fa9
```

For longer local forward streams set `AZNET_MEASURE_STREAM_BYTES=33554432`
and restrict the final filter component to `stream$`. For the read-size sweep
set `AZNET_MEASURE_READ_SIZE=1024` or `65536`. Live runs additionally need the
explicit config and account environment variables documented in the
[reference](/reference/metrics#explicit-live-measurements); credentials are never
recorded in the public artifacts. Catalogs must be checked independently of
listener cleanup. Do not compare a live run with a local run as a before/after pair.

## Write-size follow-up — 2026-10-07

A separate live sweep held the candidate code constant and varied only application
write size: 64 KiB, 256 KiB and 1 MiB. Each case delivered 4 MiB through one
session with 64 KiB application reads. Three rounds used size orders
64/256/1024, 1024/256/64 and 256/64/1024 KiB. Setup/cleanup are excluded; all 27
cases verified bytes/order/EOF, and account catalogs were unchanged afterward.
These short runs test sensitivity to request size, not sustained service capacity.
At 1 MiB, there are only four application writes per run.

Each cell is **delivered MiB/s; SDK attempts per delivered MiB** (median of three).

| Driver | 64 KiB writes | 256 KiB writes | 1 MiB writes |
| :--- | ---: | ---: | ---: |
| azblob | 0.45; 34.00 | 1.32; 9.25 | 1.24; 3.50 |
| azqueue | 0.83; 77.75 | 1.02; 59.25 | 1.03; 54.25 |
| aztable | 1.39; 28.25 | 2.89; 8.75 | 3.71; 5.00 |

The earlier candidate's single-session 64 KiB Write medians were 144 ms for
Blob, 71 ms for Queue and 40 ms for Table. A serialized 64 KiB write every
144 ms permits only about 0.43 MiB/s, close to the observed Blob rate.
`Conn.Write` flushes before returning; Blob's `WriteRaw` performs an acknowledged
AppendBlock under its TX lock. Its configured chunk ceiling is 4 MiB and the
default write/retry allowances are already 4 MiB. Raising those limits alone
does not combine consecutive small application writes. The 64-byte read size in
the preceding core experiment was a separate CPU-path parameter.

This confirms that application write/request size materially affects throughput
and request efficiency. Larger is not monotonically faster in this short sweep.
Blob uses a different account from Queue/Table, and regions remain unverified,
so the results do not establish that Blob storage is intrinsically slower.
[Azure's latency guidance](https://learn.microsoft.com/en-us/azure/storage/blobs/storage-blobs-latency)
describes request size, outstanding requests, network distance and routing as
factors in observed throughput/latency. The client measurements do not separate
service processing from network latency.

Raw [workloads](/measurements/aznet-write-size-20261007-workloads.csv) and
[request counts](/measurements/aznet-write-size-20261007-requests.csv) are retained.
Reproduce with the existing harness, explicit live account variables,
`AZNET_MEASURE_READ_SIZE=65536`, `AZNET_MEASURE_STREAM_BYTES=4194304`, each
`AZNET_MEASURE_WRITE_SIZE` above, and test filter
`^TestSDKWorkloadMeasurement$/az(blob|queue|table)/connections1$/stream$`.
This is a workload-parameter comparison, not a new implementation improvement.

A separate network control made five fresh, unauthenticated HTTPS HEAD requests
to each account's service endpoint (all returned HTTP 400; no secrets were sent).
Median TCP connection establishment, excluding DNS, was **122.0 ms** to
`premiumblob.blob.core.windows.net`, versus **25.3 ms** to
`standardacct.queue.core.windows.net` and **24.1 ms** to
`standardacct.table.core.windows.net`. Median TLS handshake intervals were
264.8 / 64.6 / 63.7 ms respectively. These are connection-startup probes, not
SDK operation timings, and established SDK connections can be reused.
Nevertheless, the TCP gap is strong evidence of a slower network path to the
Blob account; account placement/routing is a plausible explanation, not a
verified region assignment. Premium service-side storage cannot eliminate that
client-to-endpoint delay. [Raw network probes](/measurements/aznet-write-size-20261007-network.csv).

## Blob account comparison — 2026-10-07

The next control ran the same Blob driver on **both accounts**, with fresh
measurements interleaved by account. The candidate binary was unchanged. Three
rounds rotated 64/256/1024 KiB write-size order and alternated account order.
Each case delivered 4 MiB through one session with 64 KiB reads and default
buffer limits; bytes, order and EOF were verified. All 18 cases passed.

The data-plane account-info API confirmed `premiumblob` as
BlockBlobStorage/Premium_LRS and `standardacct` as StorageV2/Standard_LRS.
The latter is the “aznet” account selected from the authorized config.
This isolates the Blob implementation and workload while changing the account
and its network/service environment; it does not isolate storage tier alone.
Regions remain unverified. Short transfers are not sustained-capacity estimates.

Values are medians of three runs:

| Application write | premiumblob MiB/s | standardacct MiB/s | Throughput ratio | premiumblob attempts/MiB | standardacct attempts/MiB |
| :--- | ---: | ---: | ---: | ---: | ---: |
| 64 KiB | 0.45 | 1.71 | 3.80× | 34.75 | 36.25 |
| 256 KiB | 1.21 | 4.16 | 3.45× | 7.00 | 9.50 |
| 1 MiB | 1.36 | 5.67 | 4.17× | 3.50 | 3.50 |

Observed throughput ranges (min–max, MiB/s):

| Write | premiumblob | standardacct |
| :--- | ---: | ---: |
| 64 KiB | 0.44–0.47 | 1.69–1.75 |
| 256 KiB | 1.04–1.28 | 3.96–4.49 |
| 1 MiB | 1.30–1.43 | 5.29–5.69 |

Independent container catalogs were equal before and after on both accounts;
only uniquely named benchmark resources were created/deleted.
Raw [workloads](/measurements/aznet-blob-accounts-20261007-workloads.csv),
[SDK attempts](/measurements/aznet-blob-accounts-20261007-requests.csv), and
[lifecycle counts](/measurements/aznet-blob-accounts-20261007-lifecycle.csv) are retained.

Reproduce with the existing harness, authorized live config,
`AZNET_MEASURE_AZBLOB_ACCOUNT=premiumblob` or `standardacct`,
`AZNET_MEASURE_READ_SIZE=65536`, `AZNET_MEASURE_STREAM_BYTES=4194304`,
the three write sizes above, and filter
`^TestSDKWorkloadMeasurement$/azblob/connections1$/stream$`. The tests use
`.scratch/core-performance-20261007/after-diagnostic.test` from the earlier study.
No runtime defaults or implementation changed for this comparison.

Fresh network probes to the two **Blob** endpoints repeated the path difference:
median TCP connection establishment excluding DNS was 115.1 ms for
`premiumblob` and 24.2 ms for `standardacct` (five fresh connections each). Median
TLS handshake intervals were 250.7 and 66.1 ms. All unauthenticated HEAD
probes returned HTTP 400 and sent no secrets. Connection-startup measurements
are separate from SDK request timings; SDK connections can be reused.
[Raw probes](/measurements/aznet-blob-accounts-20261007-network.csv) are retained.

The account change increases delivered throughput by 3.45–4.17× in these
matched short runs, despite the faster account using standard storage.
Together with the TCP difference, this strongly supports network/account
conditions as a major contributor to the earlier Blob disadvantage. It does
not identify either account's region or prove standard storage inherently
outperforms premium. Request attempts per MiB did not fall with the faster
account: they rose at the smaller write sizes and were equal at 1 MiB.

## Endpoint-region verification — 2026-10-07

A subsequent read-only check mapped each Blob endpoint's live DNS address to
Microsoft's official regional Storage service tags, version 2026.10.05
(change number 421). The result is:

| Account | Endpoint IP | Matching Storage tag | Region / physical location |
| :--- | :--- | :--- | :--- |
| standardacct | 52.239.251.164 | Storage.SwitzerlandNorth (`52.239.251.0/24`) | Switzerland North / Zurich |
| premiumblob | 20.60.244.1 | Storage.CentralUS (`20.60.244.0/23`) | Central US / Iowa |

DNS also returned storage hostnames `blob.zrh22prdstr01a.store.core.windows.net`
and `blob.dm2prdstf01a.store.core.windows.net` respectively. The region conclusion
is based on the published IP prefixes, not interpreting these hostname codes.
[Microsoft's service-tag download](https://www.microsoft.com/en-us/download/details.aspx?id=56519)
provides the IP-to-region data; the
[Azure regions list](https://learn.microsoft.com/en-us/azure/reliability/regions-list)
provides the physical locations.

This geographic split is consistent with the measured 24.2 versus 115.1 ms
TCP establishment times and the matched Blob throughput difference. It strongly
supports region/network distance as a major contributor, but does not isolate
all service-side differences or prove region is the sole cause.

The local Azure CLI refresh token is expired (`AADSTS700082`), so an ARM
`location`/`primaryLocation` query was unavailable. Treat the result as verified
endpoint-IP region mapping, not direct confirmation of account configuration.
This adds endpoint-region evidence to the earlier studies, which had no region
verification at measurement time.

[Retained DNS, matching prefixes, source URL, timestamp and source checksum](/measurements/aznet-account-regions-20261007.json).
