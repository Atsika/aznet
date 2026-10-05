---
title: Metrics & Monitoring
description: SDK request attempts and transport payload measurements.
---

Azure adapters instrument each SDK pipeline attempt, including unsuccessful
responses, transport errors, SDK retries, empty polls, each pagination request,
resource creation and cleanup. Instrumentation covers shared-key and SAS clients,
including session clients and rotated Blob resources. It adds no retry policy and
does not change polling, pooling or buffering defaults.

## Request counters

`GetMetrics(conn)` returns the configured `Metrics` collector. Existing aggregate
getters remain source-compatible, but now count actual attempts rather than
successful logical driver calls:

| Getter | Attempts counted |
| --- | --- |
| `GetWriteTransactionCount` | Writes, creation and outer batch requests |
| `GetReadTransactionCount` | Reads, properties and Queue polls |
| `GetListTransactionCount` | Blob list pages and Table query pages |
| `GetDeleteTransactionCount` | Individual delete requests |

Categories are exclusive. A Table cleanup batch counts as one outer write request;
its individual actions are not additional HTTP requests. Inner batch failures can
occur inside an HTTP-success response and are surfaced by the driver, not inferred
from the outer request status. These counters are **not billing units or pricing
classes**. A transport error may leave service execution unknown; an SDK attempt
is not proof of a billable operation, or a count of lower-level network retries.

`DefaultMetrics` also implements the optional `RequestMetrics` interface:

```go
metrics := aznet.NewDefaultMetrics()
// Supply WithMetrics(metrics) when constructing the listener or dialer.
for attempt, count := range metrics.RequestCounts() {
    fmt.Println(attempt.Driver, attempt.Operation, attempt.Method,
        attempt.StatusCode, attempt.Retry, attempt.Failed, count)
}
```

`RequestCounts` returns a copied snapshot. Labels use fixed operation names and
never contain URLs, resource identifiers, credentials, or error text. Status zero
means no HTTP response. `Retry` means an SDK retry of the same operation; a later
application retry starts a new operation. `Failed` means a pipeline error or HTTP
status >= 400. Attempts are recorded on completion, so in-flight requests are
excluded. Aggregate counters and the detail snapshot are individually thread-safe,
but reading both during traffic is not an atomic combined snapshot.

Custom collectors need not implement `RequestMetrics`; they still receive the
aggregate increments. Custom drivers retain the existing Driver/Transport contract
and own any non-Azure request instrumentation. `WithMetrics` scopes the collector:
listener-created connections share the listener's collector, including bootstrap
and cleanup requests. Use separate collectors when separate totals are required.

## Transport byte counters

`GetBytesSent` counts bytes from successful transport writes and token/handshake
publication. `GetBytesReceived` counts bytes consumed from transport responses and
tokens. These are logical transport payload observations, including ciphertext and
framing; they exclude HTTP headers and encoded service envelopes. SDK retries do
not repeat the logical byte increment. An explicit application-level repeat can
count again. These counters are neither application byte counts nor network or
billable ingress/egress. Workload measurements report application bytes separately.

## Reproducing measurements

Run Azurite 3.34.0 on localhost ports 10000–10002, with `--skipApiVersionCheck` for
the pinned SDK version. The opt-in measurement uses only the public development
account, unique bootstrap/session resources and cleanup:

```sh
AZNET_MEASURE=1 GOWORK=off go test -mod=readonly \
  -run '^TestSDKWorkloadMeasurement$' -count=1 -v -timeout=12m .
```

The workload uses public Listen/Dial and net.Conn reads/writes through all three
adapters, encryption and real SDK clients. It compares 1, 4 and 16 concurrent
sessions: idle reads for 100 ms; sixteen 64-byte request/reply exchanges; four
256 KiB bulk writes; and four 32 KiB writes consumed in 1 KiB pieces with 1 ms
pauses. Byte identity is checked in both directions. Measurement-only poll settings
are 1 ms fast/accept and 10 ms data, with ping disabled; production defaults stay
unchanged. Setup and final teardown are excluded; first-read token cleanup may be
included in workload requests.

Reported elapsed time and p95 completed-operation latency include SDK, local HTTP
and emulator scheduling. Allocations and sampled peak Go heap are process-wide,
including the harness; heap is sampled every 2 ms and short peaks may be missed.
Sampling itself adds overhead. Run without race instrumentation for measurements.
The emulator's process memory is not included. These results establish a local
baseline, not cloud latency, sustained capacity, a driver cost ranking, or a reason
to tune defaults.

## Local baseline — 2026-10-05

Go 1.26.5 darwin/arm64, Azurite 3.34.0 Docker image `sha256:0a47e12e3693483cef5c71f35468b91d751611f172d2f97414e9c69113b106d9`. One run per workload; no statistical confidence or driver ranking is inferred. SDK attempts include both endpoints. Allocation bytes/counts and sampled peak heap are process-wide during the measured interval. Full raw values are retained below.

| Driver | Sessions | Workload | MiB/s | p95 µs | Allocated bytes | Allocations | Peak heap bytes | Peak heap delta | SDK attempts |
| --- | ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| azblob | 1 | idle | 0.0000 | 0 | 161760 | 2018 | 945432 | 161760 | 10 |
| azblob | 1 | interactive | 0.0115 | 14585 | 1122320 | 16621 | 1951448 | 1122320 | 65 |
| azblob | 1 | bulk | 30.3517 | 10229 | 3024816 | 2321 | 3232664 | 2228632 | 9 |
| azblob | 1 | slow | 0.6685 | 48305 | 540832 | 2779 | 2981568 | 540832 | 9 |
| azblob | 4 | idle | 0.0000 | 0 | 723080 | 8958 | 3370184 | 723080 | 44 |
| azblob | 4 | interactive | 0.0320 | 24241 | 4364664 | 66700 | 3283800 | 1561624 | 260 |
| azblob | 4 | bulk | 52.6778 | 29602 | 12052192 | 9069 | 12568000 | 10832800 | 36 |
| azblob | 4 | slow | 2.4931 | 54756 | 2023520 | 10951 | 8715880 | 2023520 | 36 |
| azblob | 16 | idle | 0.0000 | 0 | 2784960 | 34651 | 12599440 | 2784960 | 171 |
| azblob | 16 | interactive | 0.0563 | 40947 | 16534888 | 266569 | 17493056 | 7545584 | 1040 |
| azblob | 16 | bulk | 86.1016 | 64507 | 44689912 | 36984 | 47329960 | 36858392 | 144 |
| azblob | 16 | slow | 9.1792 | 61801 | 7808776 | 45178 | 40254120 | 7808776 | 144 |
| azqueue | 1 | idle | 0.0000 | 0 | 140624 | 1801 | 28781192 | 140624 | 11 |
| azqueue | 1 | interactive | 0.0117 | 12835 | 1715288 | 23173 | 4388392 | 1416216 | 97 |
| azqueue | 1 | bulk | 6.3718 | 44169 | 24053928 | 14506 | 6334528 | 4115800 | 53 |
| azqueue | 1 | slow | 0.6081 | 59680 | 3578688 | 3742 | 3899560 | 1743368 | 13 |
| azqueue | 4 | idle | 0.0000 | 0 | 555480 | 7192 | 3565288 | 555480 | 44 |
| azqueue | 4 | interactive | 0.0359 | 17944 | 7019648 | 92692 | 5592008 | 2583688 | 388 |
| azqueue | 4 | bulk | 16.3524 | 79963 | 97780184 | 56965 | 18425960 | 15360640 | 212 |
| azqueue | 4 | slow | 2.6040 | 54285 | 14503192 | 14720 | 11647368 | 4898080 | 52 |
| azqueue | 16 | idle | 0.0000 | 0 | 2190712 | 27666 | 10541352 | 2190712 | 170 |
| azqueue | 16 | interactive | 0.0381 | 59772 | 27364856 | 370948 | 15359856 | 7041696 | 1552 |
| azqueue | 16 | bulk | 16.8353 | 292164 | 390777832 | 227516 | 64197384 | 55799672 | 848 |
| azqueue | 16 | slow | 9.3843 | 78967 | 56845664 | 58850 | 45946112 | 22104952 | 208 |
| aztable | 1 | idle | 0.0000 | 0 | 168384 | 2507 | 20753048 | 168384 | 12 |
| aztable | 1 | interactive | 0.0280 | 4931 | 1218512 | 19489 | 3714680 | 1218512 | 65 |
| aztable | 1 | bulk | 8.1805 | 32296 | 37933136 | 3921 | 10757408 | 8261552 | 9 |
| aztable | 1 | slow | 0.6582 | 52010 | 4174808 | 3279 | 7077856 | 3335688 | 9 |
| aztable | 4 | idle | 0.0000 | 0 | 630336 | 9257 | 5321376 | 630336 | 44 |
| aztable | 4 | interactive | 0.0729 | 10689 | 4935216 | 78207 | 8621392 | 3737488 | 260 |
| aztable | 4 | bulk | 26.9515 | 46050 | 152331480 | 15550 | 27653792 | 23971256 | 36 |
| aztable | 4 | slow | 2.8313 | 47135 | 16962296 | 13127 | 16693872 | 9306840 | 36 |
| aztable | 16 | idle | 0.0000 | 0 | 2444120 | 36182 | 13253248 | 2444120 | 173 |
| aztable | 16 | interactive | 0.0958 | 34730 | 20038352 | 312730 | 18896024 | 8951448 | 1040 |
| aztable | 16 | bulk | 32.5427 | 309354 | 529028744 | 61318 | 90201560 | 81124352 | 144 |
| aztable | 16 | slow | 10.1074 | 63808 | 66478200 | 52471 | 61877536 | 28990960 | 144 |

Zero idle p95 denotes no completed payload operation. The explicit slow-reader delay is part of its latency. No defaults were tuned from this single local baseline.
