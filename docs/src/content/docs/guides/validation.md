---
title: Validation and Migration
description: Exact stabilized revisions, runtime evidence, and deployment requirements.
---

## Revisions and evidence

The final stabilization validation on 2026-10-06 used aznet **`7abcd80a7a2820d69e55deb1fc62f8b5a601f4be`**, published as **`v0.0.0-20261006132211-7abcd80a7a28`**, with ProxyBlob source **`6493d6329a62dc3a1ff5a2f0ca3823b5926ea46b`**. This documentation update does not change runtime code or repin that dependency.

Both the committed published dependency (`GOWORK=off`, `-mod=readonly`) and an explicit workspace using merged aznet source were validated. A separate publication check compared all 78 published module files with merged source. Workspace success alone is not release evidence.

| Check | Evidence and boundary |
| :--- | :--- |
| Go race tests and vet | Both repositories and dependency modes passed; standalone aznet selected Go 1.26.5, Proxy/workspace used Go 1.26.4 on darwin/arm64 |
| Native and WASM builds | Passed; compilation is distinct from runtime validation |
| SDK integration | Full opt-in suite passed against local Azurite 3.34.0 |
| Bun WASM runtime | Actual Go WASM socket harness passed on Bun 1.4.2 in both dependency modes, with zero owned callbacks/handles/sockets at completion |
| Node adapter runtime | Deterministic adapter tests passed on Node 26.3.0; this is not live Azure WASM evidence |
| Native live Azure | Blob, Queue and Table each passed 3 BIND conversations, 3 public DNS queries and 45 UDP echo cases; independent catalog checks found zero owned resources before and after |
| Direct aznet live checks | Bootstrap deletion/reuse across all three drivers and Table reclamation with SAS/account-key receivers passed under race instrumentation |

Direct Table checks covered consumed-row batching, uncertain responses, unchanged retry ciphertext, missing-row reconciliation and preservation of the latest receipt. Bootstrap tests observed deletion-in-progress responses and eventual name reuse rather than treating a successful delete call as immediate reuse.

The [retained release report](https://github.com/quarkslab/proxyblob/blob/0487894/docs/release-validation.md) records commands and limitations. Subsequent [real active FTP validation](https://github.com/quarkslab/proxyblob/blob/444ce3995aa63ced48feb57145b8fc4208dd4638/docs/active-ftp-bind.md) used tnftp/Dante/vsftpd through TCP and each Azure driver, including EPRT/PORT, listings and byte-verified 2 MiB transfers. This validates an application over ProxyBlob, not every possible net.Conn consumer.

## Local checks

```bash
GOWORK=off go test -mod=readonly -race -count=1 ./...
GOWORK=off go vet -mod=readonly ./...
GOWORK=off go build -mod=readonly ./...
GOOS=js GOARCH=wasm GOWORK=off go build -mod=readonly ./...
```

These commands alone skip external-service tests. Use the [Azurite setup](/guides/azurite) for the opt-in SDK suite. Live tests need authorized, isolated test namespaces and independent cleanup verification; do not run destructive cleanup against a shared bootstrap namespace.

## Upgrade requirements

- Custom drivers and wrappers must implement sequence-aware `WriteRaw(ctx, seq, data)`, preserve optional capabilities and satisfy the [retry/body/cleanup contract](/guides/developing-drivers#migration-checklist).
- Upgrade Table listeners before clients that require reclamation. New response SAS permissions include Delete; existing read-only sessions cannot be upgraded in place.
- Namespace administrators now own explicit `CleanupBootstrap` after every user has stopped. Listener Close only tears down its owned sessions.
- Handle positive accepted Write counts with an error correctly: retry only the unaccepted suffix. Use half-close and an application acknowledgement when final delivery matters.
- Configure bootstrap and session lifetime separately as needed. There is no automatic renewal or session resume, and expiry metadata is not a liveness guarantee.
- Assess memory limits, polling and service costs at the intended concurrency. Per-connection buffers do not constitute an application-wide admission budget or a remote-storage cap.

## Remaining deployment validation

Run a sustained soak at expected concurrency, region and account settings. Exercise credentials expiring during active sessions, outages and process crashes, and independently reconcile owned resources. The retained native Azure tests and Bun harness do not establish Azure behavior inside every production WASM host. Validate that host and its lifecycle explicitly before rollout. No universal throughput or latency SLO follows from these finite tests.
