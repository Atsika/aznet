---
title: Your First Connection
description: Run a complete request and response locally before connecting to Azure.
---

Start with a local storage emulator and the runnable `examples/quickstart` program. The server listens through storage, the client sends `hello aznet`, and both sides finish before the server removes the demo resources.

## 1. Prepare the tools

You need Git, Docker and Go with automatic toolchain selection enabled. This revision selects Go 1.26.5. Bun is only needed to develop the documentation site, not to use the Go library.

```sh
git clone https://github.com/atsika/aznet.git
cd aznet
```

In a separate terminal, start the disposable emulator:

```sh
docker run --rm --name aznet-demo \
  -p 127.0.0.1:10000:10000 -p 127.0.0.1:10001:10001 -p 127.0.0.1:10002:10002 \
  mcr.microsoft.com/azure-storage/azurite:3.34.0 \
  azurite --blobHost 0.0.0.0 --queueHost 0.0.0.0 --tableHost 0.0.0.0 --skipApiVersionCheck
```

Wait until the emulator reports that its services are listening. The version-check bypass is needed by the SDK used here; this local test is not evidence for every Azure service behavior.

## 2. Start the server

In the repository directory, set the **public emulator credentials** and start the Blob example:

```sh
export AZURE_STORAGE_ACCOUNT=devstoreaccount1
export AZURE_STORAGE_ACCOUNT_KEY='Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=='
go run ./examples/quickstart \
  -listen http://127.0.0.1:10000/devstoreaccount1
```

The server prints a fresh connection URL and `Waiting for one client`. Leave it running. A connection URL contains bearer credentials: copy the complete URL only to the intended client.

## 3. Run the client

Open another terminal in the same repository. Read the URL into an environment variable, paste it at the prompt and press Enter:

```sh
read -r AZNET_URL
export AZNET_URL
go run ./examples/quickstart
unset AZNET_URL
```

Expected client output:

```text
hello aznet
```

The server reports `Round trip acknowledged; cleaning up`, then exits. Both processes should exit successfully. The demo reserves the `aznetdemohandshake` and `aznetdemotoken` namespace; run one demo at a time, or pass the same unique lowercase `-namespace` value to both sides.

## 4. Finish and clean up

The server deletes its session and its exclusive demo bootstrap resources. Stop the disposable emulator when finished:

```sh
docker stop aznet-demo
```

Do not apply the demo's bootstrap cleanup to shared production namespaces. Ordinary listener Close leaves bootstrap resources intact; their administrator removes them after all users stop. If the demo reports incomplete cleanup or is killed forcibly, inspect its resources or remove its disposable emulator. A cleanup timeout does not prove deletion.

## Try another driver

Pass the same `-driver` to both processes and use the matching server endpoint:

| Driver | Local server endpoint | Azure server endpoint |
|---|---|---|
| `azblob` | `http://127.0.0.1:10000/devstoreaccount1` | `https://ACCOUNT.blob.core.windows.net` |
| `azqueue` | `http://127.0.0.1:10001/devstoreaccount1` | `https://ACCOUNT.queue.core.windows.net` |
| `aztable` | `http://127.0.0.1:10002/devstoreaccount1` | `https://ACCOUNT.table.core.windows.net` |

For example, the server uses `-driver azqueue -listen http://127.0.0.1:10001/devstoreaccount1`, and the client uses `-driver azqueue`. Generate a fresh URL for each server run.

## Move to Azure

Use a storage account with the selected service, allow authorized network access from both endpoints, and set the server's `AZURE_STORAGE_ACCOUNT` and `AZURE_STORAGE_ACCOUNT_KEY` to its credentials. Replace the local `-listen` URL using the table above. The client still needs only the generated URL and matching driver/namespace, not the account key.

Standard general-purpose v2 supports all three services. Premium Blob is not required. Anonymous public Blob access is not required either. See [driver selection](/drivers/overview) and [security and authorization](/core-concepts/security) before deployment. Use a dedicated demo namespace: the example removes it when done.

## Use aznet in your application

```sh
go get github.com/atsika/aznet
```

`Listen` returns `net.Listener`; `Dial` and `Accept` return `net.Conn`. Start from the [quickstart source](https://github.com/atsika/aznet/tree/main/examples/quickstart), then read the [API reference](/reference/api) for deadlines, accepted write counts and resource ownership.

The example half-closes the response, waits for a client acknowledgement and ordered EOF, then performs full Close. A successful upload alone does not mean the peer consumed the response. Plan the same application-level completion in delivery-sensitive protocols.

## If it does not work

| Symptom | Check |
|---|---|
| Server cannot listen | Emulator is ready; ports are free; server endpoint and account key match |
| Demo namespace is still being deleted | Wait and retry, or use a new exclusive `-namespace` with the same value on server and client |
| Client cannot dial | Copy a fresh complete URL; keep the server running; match driver and namespace |
| Azure rejects authorization | Verify credentials, service permissions, network access and expiry; avoid posting raw SDK errors or URLs |
| Transfer or acknowledgement times out | Both processes have access to storage; the example has a 30-second conversation deadline |
| Cleanup is incomplete | Inspect the exclusive demo namespace; do not delete other applications' resources |

For existing deployments, read [validation and migration](/guides/validation). For expected latency and cost, use the [measurement guide](/drivers/performance), rather than assuming TCP-like speed.
