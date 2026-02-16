---
title: Metrics & Monitoring
description: How to track and collect connection statistics in aznet.
---

`aznet` provides a built-in metrics system to monitor API transactions and data transfer, which is essential for estimating Azure Storage costs.

## Metrics Interface

The `Metrics` interface tracks granular operations that map to Azure's pricing model.

```go
type Metrics interface {
    // Transaction counters (mapped to pricing classes)
    IncrementWriteTransaction()  // Class A
    IncrementReadTransaction()   // Class B
    IncrementListTransaction()   // Class A
    IncrementDeleteTransaction() // Class A
    
    // Data transfer counters (in bytes)
    IncrementBytesSent(n int64)     // Ingress
    IncrementBytesReceived(n int64) // Egress

    // Getters for all counters
    GetWriteTransactionCount() int64
    GetReadTransactionCount() int64
    GetListTransactionCount() int64
    GetDeleteTransactionCount() int64
    GetBytesSent() int64
    GetBytesReceived() int64
}
```

## Default Implementation

By default, `aznet` uses `DefaultMetrics`, which employs atomic counters.

### Transaction Mapping

| aznet Method                 | Azure Storage Pricing Class | Operations                                                  |
| :--------------------------- | :-------------------------- | :---------------------------------------------------------- |
| `IncrementWriteTransaction`  | **Class A**                 | `Put`, `Post`, `AppendBlock`, `AddEntity`, `EnqueueMessage` |
| `IncrementReadTransaction`   | **Class B**                 | `Get`, `Head`, `PeekMessages`, `GetProperties`              |
| `IncrementListTransaction`   | **Class A**                 | `ListBlobs`, `QueryEntities`                                |
| `IncrementDeleteTransaction` | **Class A**                 | `Delete`                                                    |

### Data Transfer

- **Bytes Sent**: Tracks data uploaded to Azure (Ingress). Ingress is usually free in most Azure regions.
- **Bytes Received**: Tracks data downloaded from Azure (Egress). Egress is charged when data leaves an Azure region.

## Cost Estimation Guide

To estimate your connection cost:

1. Retrieve metrics via `aznet.GetMetrics(conn)`.
2. Multiply Class A transactions (Write + List + Delete) by your region's Class A price (see tables below).
3. Multiply Class B transactions (Read) by your region's Class B price (see tables below).
4. Multiply `BytesReceived` (in GB) by the internet egress rate if your client is outside Azure.

> **Note**: Polling (checking for new data) increments `ReadTransaction` even if no data is found. `aznet` uses an adaptive poller to minimize these costs during idle periods.

### Pricing Reference

The following tables show pricing in EUR (€) for various Azure regions and tiers. Prices are subject to change; always check the official Azure Storage pricing page for the most up-to-date rates.

#### 1. Blob Storage Operations (per 10,000)

| Operation Type                 | Premium | Hot     | Cool    | Cold    | Archive |
| :----------------------------- | :------ | :------ | :------ | :------ | :------ |
| **Write operations** (Class A) | €0.0243 | €0.0655 | €0.1216 | €0.1989 | €0.1381 |
| **Read operations** (Class B)  | €0.0020 | €0.0051 | €0.0119 | €0.1105 | €6.9050 |
| **Iterative Read**             | €0.0243 | €0.0052 | €0.0052 | €0.0052 | €0.0052 |
| **Data Retrieval** (per GB)    | Free    | Free    | €0.0094 | €0.0255 | €0.0213 |
| **All other** (except Delete*) | €0.0020 | €0.0051 | €0.0051 | €0.0051 | €0.0051 |

*\* Delete operations are free.*

#### 2. Table Storage Operations (per 10,000)

| Operation       | LRS     | GRS / RA-GRS | ZRS     | GZRS / RA-GZRS    |
| :-------------- | :------ | :----------- | :------ | :---------------- |
| **Batch Write** | €0.0798 | €0.1598      | €0.0798 | €0.1833 / €0.1598 |
| **Write**       | €0.0266 | €0.0532      | €0.0266 | €0.0612 / €0.0532 |
| **Read**        | €0.0054 | €0.0054      | €0.0054 | €0.0054           |
| **Scan / List** | €0.0952 | €0.0952      | €0.0952 | €0.0952           |
| **Delete**      | €0      | €0           | €0      | €0                |

#### 3. Queue Storage Operations (per 10,000)

| Operation Class              | LRS     | GRS / RA-GRS |
| :--------------------------- | :------ | :----------- |
| **Class 1***                 | €0.0004 | €0.0004      |
| **Class 2****                | €0.0004 | €0.0004      |
| **Geo-replication** (per GB) | N/A     | Free         |

*\* **Class 1**: CreateQueue, ListQueues, PutMessage, SetQueueMetadata, UpdateMessage, ClearMessages, DeleteMessage, DeleteQueue, GetMessageWrite, GetMessagesWrite.*
\* **Class 2**: GetMessage, GetMessages, GetQueueMetadata, GetQueueServiceProperties, GetQueueAcl, PeekMessage, PeekMessages, GetMessageRead, GetMessagesRead.*

## Why Monitor?

Monitoring API calls is critical for:

1. **Cost Control**: Every poll and every data chunk is an Azure API call.
2. **Performance Debugging**: High request counts for small amounts of data might indicate inefficient buffering.
3. **Health Checking**: Monitoring the ratio of transactions to data transferred can help detect connection issues.
