---
title: Performance Analysis
description: A detailed comparison of throughput, latency, and efficiency across different Azure Storage drivers.
---

Performance in `aznet` is primarily determined by the **chunk size** supported by the underlying Azure service.
Larger chunks mean fewer API calls and less accumulated network latency.

:::note
The following benchmarks were performed using **iperf3** through an aznet SOCKS proxy against a public iperf server, measuring real-world throughput over live Azure infrastructure.
:::

## Summary

| Driver      | Sender        | Receiver       | Speed Ranking |
| :---------- | :------------ | :------------- | :------------ |
| **azblob**  | **3.07 MB/s** | **2.65 MB/s**  | **Fastest**   |
| **azqueue** | 1.11 MB/s     | 0.79 MB/s      | Medium        |
| **aztable** | 1.13 MB/s     | 0.54 MB/s      | Slowest       |

## Key Performance Drivers

### 1. Storage Account Type

Azure Blob Storage performance varies significantly between account types:

- **Standard**: General-purpose storage using HDD-based media. Good for bulk data.
- **Premium**: High-performance storage using SSD-based media. Significantly lower latency for small operations and higher throughput for large transfers.

### 2. Chunk Size

The most significant factor. Azure Blob Storage supports the largest appends, dramatically reducing the number of round-trips.

- **azblob**: 4,096 KB chunks.
- **aztable**: 960 KB chunks (using multi-property entities).
- **azqueue**: 48 KB chunks (85x more operations than Blob).

### 3. Transfer Pattern

Each driver exhibits a different transfer behavior:

- **azblob**: Smooth, consistent throughput with zero retransmissions.
- **azqueue**: Steady flow with occasional minor retransmissions.
- **aztable**: Bursty pattern with frequent zero-transfer intervals and high retransmissions, caused by the heavier per-operation overhead of entity serialization and query-based reads.

## iperf3 Benchmark Details

| Driver      | Sender (Mbits/sec) | Receiver (Mbits/sec) | Retransmissions | Pattern |
| :---------- | :------------------ | :------------------- | :-------------- | :------ |
| **azblob**  | **24.6**            | **21.2**             | 0               | Smooth  |
| **azqueue** | 8.91                | 6.31                 | 3               | Steady  |
| **aztable** | 9.02                | 4.30                 | 16              | Bursty  |

*Benchmarks performed through an aznet SOCKS proxy to a public iperf3 server.*

## Operation Efficiency (100 MB Echo Transfer)

Operation counts measured from a live Azure 100 MB echo transfer using the metrics example.

| Driver      | Total Write Ops | Total Read Ops | Total List Ops | Grand Total |
| :---------- | :-------------- | :------------- | :------------- | :---------- |
| **azblob**  | 55              | 47             | 24             | **128**     |
| **azqueue** | 4,274           | 583            | 182            | **5,041**   |
| **aztable** | 217             | 207            | 84             | **510**     |

*Combined client + server operations. `azblob` requires ~39x fewer total operations than `azqueue` for the same data.*
