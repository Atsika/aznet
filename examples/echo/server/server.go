package main

import (
	"io"
	"log"
	"net"

	"github.com/atsika/aznet"
)

// Driver to use (azblob, azqueue, aztable)
var driver = "azblob"

// Blob URL
var u = "http://devstoreaccount1:Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq%2FK1SZFPTOtr%2FKBHBeksoGMGw%3D%3D@localhost:10000/devstoreaccount1"

// Queue URL
// var u = "http://devstoreaccount1:Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq%2FK1SZFPTOtr%2FKBHBeksoGMGw%3D%3D@localhost:10001/devstoreaccount1"

// Table URL
// var u = "http://devstoreaccount1:Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq%2FK1SZFPTOtr%2FKBHBeksoGMGw%3D%3D@localhost:10002/devstoreaccount1"

// Echo server that works with blob, queue, and table transports.
// Start Azurite with docker-compose (see project root), then run this server.
// The server will listen for connections and echo back any data it receives.
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
			if _, err := io.Copy(c, c); err != nil {
				log.Printf("echo error: %v", err)
			}
		}(conn)
	}
}
