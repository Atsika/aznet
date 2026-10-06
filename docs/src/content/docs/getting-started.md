---
title: Getting Started
description: Learn how to install and start using aznet in your Go projects.
---

`aznet` provides a TCP-like networking abstraction over Azure Storage, allowing you to use
familiar socket programming patterns with cloud storage as the underlying transport.

## Installation

To start using `aznet` in your Go project, install the package:

```bash
go get github.com/atsika/aznet
```

## Prerequisites

- **Go with automatic toolchain selection**: this revision declares Go 1.25 and selects Go 1.26.5.
- An **Azure Storage Account** (or [Azurite](https://github.com/Azure/Azurite) for local development).

## Azure Storage Setup

Before using `aznet`, you need to set up an Azure Storage account.

### 1. Create a Storage Account

1. Go to the [Azure Portal](https://portal.azure.com).
2. Create a new **Storage account**.
3. Choose a name and region. For best performance, choose the region closest to your application.
4. Select a **Standard general-purpose v2** account to use Blob, Queue or Table. Premium block blob accounts also support append blobs but not Queue or Table. Choose redundancy for your availability needs and compare actual workload costs; no particular tier has a guaranteed aznet speedup. See [driver deployment requirements](/drivers/overview).

### 2. Get Connection Credentials

You need the storage account name and one of its access keys.

1. In your storage account, go to **Security + networking** > **Access keys**.
2. Copy the **Storage account name** and **Key1**.

### 3. Required Settings

`aznet` automatically manages the required resources (containers, queues, or tables). However, ensure your storage account allows:

- **Network access**: Both endpoints need authorized HTTPS access to storage. Anonymous public Blob access is not required.
- **Firewall**: If you use the storage firewall, ensure the IP addresses of your clients/servers are whitelisted.

## Quick Start

### 1. Server (Listener)

The server listens for incoming connections. It uses an `Account Key` for authentication.

```go
package main

import (
    "io"
    "log"
    "net"
    "github.com/atsika/aznet"
)

func main() {
    // Arguments: driver type, service URL
    // Credentials can be in the URL or in environment variables
    address := "https://myaccount:mykey@myaccount.blob.core.windows.net/"
    
    listener, err := aznet.Listen("azblob", address)
    if err != nil {
        log.Fatal("storage operation failed; inspect the error securely")
    }
    defer listener.Close()
    
    log.Println("Listening on Azure Storage...")

    for {
        conn, err := listener.Accept()
        if err != nil {
            log.Print("Accept failed; inspect and redact the error before logging details")
            return
        }
        
        go handleConnection(conn)
    }
}

func handleConnection(conn net.Conn) {
    defer conn.Close()
    io.Copy(conn, conn) // Echo back
}
```

### 2. Client (Dialer)

The client connects to the server. Establishing a connection requires a URL with embedded SAS tokens. You can generate this URL either through the [`azurl` tool](/tools/azurl) or directly from the `Listener` in your code.

#### Using code (Server-side)

```go
// Generate a connection string for clients directly from the listener
connStr, err := listener.(*aznet.Listener).ConnectionString()
if err == nil {
    fmt.Println("Client URL:", connStr)
}
```

#### Using the `azurl` tool

The server administrator typically runs `azurl` to generate a connection string for the client.

```go
package main

import (
    "fmt"
    "log"
    "github.com/atsika/aznet"
)

func main() {
    // The server will typically provide the connection URL via azurl or ConnectionString()
    // Format: https://<host>/?handshake=<base64_sas>&token=<base64_sas>
    address := "https://myaccount.blob.core.windows.net/?handshake=YmxvYl_...&token=YmxvYl_..."
    
    conn, err := aznet.Dial("azblob", address)
    if err != nil {
        log.Fatal("storage operation failed; inspect the error securely")
    }
    defer conn.Close()
    
    if _, err := conn.Write([]byte("Hello, aznet!")); err != nil {
        log.Print("write failed")
        return
    }
    
    response := make([]byte, 1024)
    n, err := conn.Read(response)
    if err != nil {
        log.Print("read failed")
        return
    }
    fmt.Printf("Received: %s\n", response[:n])
}
```

## Interface compatibility and delivery

The returned values implement `net.Listener` and `net.Conn`, so applications can use familiar I/O APIs. Test application timeouts, write sizes, authorization lifetime and closure against storage behavior; matching interfaces do not promise TCP latency or delivery semantics.

A successful write accepts bytes locally; even a successful upload does not prove peer consumption. Full Close on an accepted connection deletes session resources. For a final response that must be delivered, half-close with `CloseWrite`, wait for application-level completion, then Close. The echo snippets are introductory examples, not a graceful production shutdown policy. See [ownership and deadlines](/reference/api#closing-and-resource-ownership).

Treat generated connection URLs as bearer secrets and share them through a protected channel. Bootstrap resources outlive listener Close and need explicit administrator cleanup. Review [migration and validation](/guides/validation) before upgrading existing peers.

## Next Steps

Check out the [Architecture](/core-concepts/architecture) to understand how `aznet` manages connections
and the [Drivers](/drivers/overview) guide to choose the right driver for your needs.
