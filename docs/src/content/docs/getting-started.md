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

- **Go 1.25** or later.
- An **Azure Storage Account** (or [Azurite](https://github.com/Azure/Azurite) for local development).

## Azure Storage Setup

Before using `aznet`, you need to set up an Azure Storage account.

### 1. Create a Storage Account

1. Go to the [Azure Portal](https://portal.azure.com).
2. Create a new **Storage account**.
3. Choose a name and region. For best performance, choose the region closest to your application.
4. **Select the correct Performance and Account Type based on your driver:**

| Driver      | Performance  | Account Type       | Storage Type | Recommendation                                                 |
| :---------- | :----------- | :----------------- | :----------- | :------------------------------------------------------------- |
| **azblob**  | **Premium**  | **Block Blobs**    | SSD          | **Recommended for production.** Up to 3x faster than Standard. |
| **azblob**  | Standard     | General Purpose v2 | HDD          | Lower performance, but works.                                  |
| **azqueue** | **Standard** | General Purpose v2 | HDD          | **Required.** Premium Block Blobs do not support Queues.       |
| **aztable** | **Standard** | General Purpose v2 | HDD          | **Required.** Premium Block Blobs do not support Tables.       |

:::tip[Best Practice]
For the best performance/cost ratio, use **different storage accounts** for blobs and queues/tables. Use a **Premium Block Blob** account for `azblob` and a **Standard General Purpose v2** account for `azqueue` or `aztable`.
:::

5. For **Redundancy**, **LRS (Locally-redundant storage)** is usually sufficient and most cost-effective for `aznet`.

### 2. Get Connection Credentials

You need the storage account name and one of its access keys.

1. In your storage account, go to **Security + networking** > **Access keys**.
2. Copy the **Storage account name** and **Key1**.

### 3. Required Settings

`aznet` automatically manages the required resources (containers, queues, or tables). However, ensure your storage account allows:

- **Public access**: While `aznet` uses SAS tokens, the account itself must be accessible over HTTPS.
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
        log.Fatal(err)
    }
    defer listener.Close()
    
    log.Println("Listening on Azure Storage...")

    for {
        conn, err := listener.Accept()
        if err != nil {
            log.Printf("Accept error: %v", err)
            continue
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
        log.Fatal(err)
    }
    defer conn.Close()
    
    conn.Write([]byte("Hello, aznet!"))
    
    response := make([]byte, 1024)
    n, _ := conn.Read(response)
    fmt.Printf("Received: %s\n", response[:n])
}
```

## Drop-in Replacement

`aznet` is designed to be a drop-in replacement for Go's standard `net` package.
Because it implements the same interfaces, you can swap traditional TCP networking for Azure Storage transport
with just a one-line change.

Using standard TCP:

```go
// Traditional TCP listener
listener, err := net.Listen("tcp", ":8080")
```

Using aznet:

```go
// aznet listener - same interface, different transport
listener, err := aznet.Listen("azblob", "https://account:key@account.blob.core.windows.net/")
```

Any code that accepts a `net.Listener` or `net.Conn` (like `http.Serve`, `grpc.Server`, or custom protocols) will continue to work without modification.

## Next Steps

Check out the [Architecture](/core-concepts/architecture) to understand how `aznet` manages connections
and the [Drivers](/drivers/overview) guide to choose the right driver for your needs.
