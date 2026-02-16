package main

import (
	"fmt"
	"io"
	"log"
	"net"

	"github.com/atsika/aznet"
)

// Driver to use
var driver = "azblob"

// Blob URL
var u = "http://devstoreaccount1:Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq%2FK1SZFPTOtr%2FKBHBeksoGMGw%3D%3D@localhost:10000/"

// Queue URL
// var u = "http://devstoreaccount1:Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq%2FK1SZFPTOtr%2FKBHBeksoGMGw%3D%3D@localhost:10001/"

// Table URL
// var u = "http://devstoreaccount1:Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq%2FK1SZFPTOtr%2FKBHBeksoGMGw%3D%3D@localhost:10002/"

// Metrics server that echoes back any data it receives while tracking metrics.
func main() {
	listener, err := aznet.Listen(driver, u)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	connStr, err := listener.(*aznet.Listener).ConnectionString()
	if err != nil {
		log.Fatalf("connection string: %v", err)
	}
	log.Printf("[aznet] connection string:\n%s\n", connStr)
	log.Println("[aznet] waiting for connections...")
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}

		go func(c net.Conn) {
			defer c.Close()
			log.Printf("[aznet] client %s connected", c.RemoteAddr())

			// Use the connection's maximum payload size for optimal buffer allocation
			bufSize := c.(*aznet.Conn).MTU()
			buf := make([]byte, bufSize)

			// Echo back any data received using the buffer
			n, err := io.CopyBuffer(c, c, buf)
			if err != nil && err != io.EOF {
				log.Printf("echo error: %v", err)
			}
			log.Printf("[aznet] echoed %d bytes to %s", n, c.RemoteAddr())

			// Log final metrics for this connection
			if connMetrics := aznet.GetMetrics(c); connMetrics != nil {
				fmt.Println("\n=== SERVER METRICS REPORT ===")
				fmt.Printf("Write Transactions:  %d\n", connMetrics.GetWriteTransactionCount())
				fmt.Printf("Read Transactions:   %d\n", connMetrics.GetReadTransactionCount())
				fmt.Printf("List Transactions:   %d\n", connMetrics.GetListTransactionCount())
				fmt.Printf("Delete Transactions: %d\n", connMetrics.GetDeleteTransactionCount())
				fmt.Printf("Bytes Sent:          %d\n", connMetrics.GetBytesSent())
				fmt.Printf("Bytes Received:      %d\n", connMetrics.GetBytesReceived())
				fmt.Println("==============================")
			}
		}(conn)
	}
}
