---
title: Cost Analysis
description: A detailed comparison of operational costs across different Azure Storage drivers.
---

Operational costs for `aznet` depend on the number and type of API calls made to Azure Storage.
This page provides a breakdown based on **Azure Europe region pricing (LRS)**.

## Pricing Fundamentals

Azure Storage billing is primarily based on the number of operations (API calls).

| Service        | Operation Type      | Cost per 10,000 ops | Notes                |
| :------------- | :------------------ | :------------------ | :------------------- |
| **Queue**      | All operations      | **€0.0004**         | Dramatically cheaper |
| **Blob (Hot)** | Write (AppendBlock) | €0.0659             | 165x more than Queue |
| **Blob (Hot)** | Read (Get/Download) | €0.0052             | 13x more than Queue  |
| **Table**      | Write (AddEntity)   | €0.0267             | 67x more than Queue  |

## Measured Operations (100 MB Echo Transfer)

The following metrics were collected from a live Azure 100 MB echo transfer using the metrics example.

### Client-Side

| Driver      | Write Txn | Read Txn | Bytes Sent  | Bytes Received |
| :---------- | :-------- | :------- | :---------- | :------------- |
| **azblob**  | 27        | 16       | 104,858,318 | 104,858,683    |
| **azqueue** | 2,137     | 331      | 104,911,068 | 104,911,377    |
| **aztable** | 108       | 99       | 104,860,343 | 104,860,764    |

### Server-Side

| Driver      | Write Txn | Read Txn | List Txn | Delete Txn | Bytes Sent  | Bytes Received |
| :---------- | :-------- | :------- | :------- | :--------- | :---------- | :------------- |
| **azblob**  | 28        | 31       | 24       | 2          | 104,858,683 | 104,858,275    |
| **azqueue** | 2,137     | 252      | 182      | 2          | 104,911,377 | 104,911,025    |
| **aztable** | 109       | 108      | 84       | 2          | 104,860,764 | 104,860,300    |

### Total Operations (Client + Server)

| Driver      | Total Write | Total Read | Total List | Total Delete | **Grand Total** |
| :---------- | :---------- | :--------- | :--------- | :----------- | :-------------- |
| **azblob**  | 55          | 47         | 24         | 2            | **128**         |
| **azqueue** | 4,274       | 583        | 182        | 2            | **5,041**       |
| **aztable** | 217         | 207        | 84         | 2            | **510**         |

## Cost Calculation (100 MB Echo Transfer)

| Driver      | Write Cost   | Read Cost    | List Cost    | Total Cost      | Relative     |
| :---------- | :----------- | :----------- | :----------- | :-------------- | :----------- |
| **azblob**  | €0.0003623   | €0.0000244   | €0.0000158   | **€0.0004025**  | 1.2x         |
| **azqueue** | €0.0001710   | €0.0000233   | €0.0000073   | **€0.0002016**  | **1.0x**     |
| **aztable** | €0.0005794   | €0.0001118   | €0.0000800   | **€0.0007712**  | 3.8x         |

*Delete operations are free for all services.*

## Why Chunk Size Drives Cost

The key insight is that **larger chunks mean fewer API calls** for the same amount of data.
`azblob` uses 4 MB chunks (only 55 total writes for 100 MB round-trip), while `azqueue` uses 48 KB chunks (4,274 writes).
Despite Queue's dramatically lower per-operation cost, `azblob` stays competitive for large transfers because it needs **~78x fewer write operations**.

## Cost at Scale

Projected costs for processing **1 TB/day** (echo round-trip, based on measured metrics):

| Driver      | Daily Cost | Annual Cost | Savings vs Table      |
| :---------- | :--------- | :---------- | :-------------------- |
| **azqueue** | **€2.06**  | **€752**    | **€5,128/year**       |
| **azblob**  | €4.12      | €1,504      | €4,376/year           |
| **aztable** | €7.89      | €2,880      | -                     |

## Recommendations

### 1. Choose `azqueue` for Batch Jobs

If your data transfer is not time-sensitive (e.g., nightly backups, log shipping),
`azqueue` provides the lowest possible cost.

### 2. Choose `azblob` for User-Facing Apps

`azblob` costs roughly 2x more than `azqueue` per transfer, but offers
**~3x better throughput** (3.07 MB/s vs 1.11 MB/s), making it the better choice for interactive applications.

### 3. Avoid `aztable`

`aztable` is the most expensive driver at **3.8x the cost of `azqueue`** with the lowest receiver throughput (0.54 MB/s).

:::caution
Pricing varies by region and storage redundancy (LRS vs GRS). Always check the latest [Azure Storage pricing](<https://azure.microsoft.com/pricing/details/storage/>) for your specific region.
:::
