package main

import (
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/atsika/aznet"
)

// Driver to use
var driver = "azblob"

// Blob URL (Azurite example)
var u = "http://localhost:10000/devstoreaccount1?handshake=c2U9MjAyNi0wMi0wOVQxMSUzQTIxJTNBNTFaJnNpZz1VN0JhWUklMkJSamY1N01Fc1NaUzFqSjV5QUYlMkZGempPWjlWdXhNYmJNZ054WSUzRCZzcD1hY3cmc3ByPWh0dHBzJTJDaHR0cCZzcj1jJnN0PTIwMjYtMDItMDhUMTElM0ExNiUzQTUxWiZzdj0yMDI1LTExLTA1&token=c2U9MjAyNi0wMi0wOVQxMSUzQTIxJTNBNTFaJnNpZz1EbENDJTJGVlYxSDBzQWI1cXI2ZjQlMkIlMkJTa3FkTHNWVmdBQSUyRm5WazA2QkpxdGMlM0Qmc3A9cmwmc3ByPWh0dHBzJTJDaHR0cCZzcj1jJnN0PTIwMjYtMDItMDhUMTElM0ExNiUzQTUxWiZzdj0yMDI1LTExLTA1"

// Queue URL
// var u = "http://localhost:10001/devstoreaccount1?handshake=c2U9MjAyNi0wMS0xN1QxNSUzQTAzJTNBMjRaJnNpZz0zS3hRaVRlN0NkUGxzeXJIMWNibE1Hb1Mwb0FMTW9iOTFVaUtiYUgzaSUyQmMlM0Qmc3A9YSZzcHI9aHR0cHMlMkNodHRwJnN0PTIwMjYtMDEtMTZUMTQlM0E1OCUzQTI0WiZzdj0yMDIwLTAyLTEw&token=c2U9MjAyNi0wMS0xN1QxNSUzQTAzJTNBMjRaJnNpZz02VUtkSTdIM3MwWnB6OTNTM3ZXVnpqeVQ4MnMwOVFSNUZ0N2tYamRRYzI0JTNEJnNwPXImc3ByPWh0dHBzJTJDaHR0cCZzdD0yMDI2LTAxLTE2VDE0JTNBNTglM0EyNFomc3Y9MjAyMC0wMi0xMA%3D%3D"

// Table URL
// var u = "http://localhost:10002/devstoreaccount1?handshake=c2U9MjAyNi0wMS0xN1QxNSUzQTI1JTNBNTFaJnNpZz04cDQ2REpYSGxJek1vZG8lMkZMclhpRDd3RnZ2SHUlMkJ6NUVVT1g4b3JIUWpLbyUzRCZzcD1hJnNwcj1odHRwcyUyQ2h0dHAmc3Q9MjAyNi0wMS0xNlQxNSUzQTIwJTNBNTFaJnN2PTIwMTktMDItMDImdG49aGFuZHNoYWtl&token=c2U9MjAyNi0wMS0xN1QxNSUzQTI1JTNBNTFaJnNpZz16SHZQZ1U2TlBYN3l6R1VocWpaTkhmWWtCc2dKVDNtMktPRUtNZTJqJTJCWjQlM0Qmc3A9ciZzcHI9aHR0cHMlMkNodHRwJnN0PTIwMjYtMDEtMTZUMTUlM0EyMCUzQTUxWiZzdj0yMDE5LTAyLTAyJnRuPXRva2Vu"

const totalData = 100 * 1024 * 1024 // 100MB

// Metrics client that sends 100MB of random data and reads it back.
func main() {
	conn, err := aznet.Dial(driver, u)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	fmt.Println("[aznet] connected")
	fmt.Printf("[aznet] local  → %s\n", conn.LocalAddr())
	fmt.Printf("[aznet] remote → %s\n", conn.RemoteAddr())

	fmt.Printf("[aznet] sending %d MB of random data...\n", totalData/(1024*1024))

	start := time.Now()

	// Use the connection's maximum payload size for optimal buffer allocation
	bufSize := conn.(*aznet.Conn).MTU()
	buf := make([]byte, bufSize)

	// Use a pipe to stream random data to the connection
	doneWrite := make(chan struct{})
	go func() {
		defer close(doneWrite)
		// Send data in chunks
		_, err := io.CopyBuffer(conn, io.LimitReader(rand.Reader, int64(totalData)), buf)
		if err != nil {
			log.Printf("write error: %v", err)
		}
	}()

	// Read echoed data back
	n, err := io.CopyBuffer(io.Discard, io.LimitReader(conn, int64(totalData)), buf)
	if err != nil && err != io.EOF {
		log.Printf("read error: %v", err)
	}

	<-doneWrite

	duration := time.Since(start)
	fmt.Printf("[aznet] transfer complete: %d bytes in %v (%.2f MB/s)\n", n, duration, float64(n)/(1024*1024)/duration.Seconds())

	// Final metrics report
	if connMetrics := aznet.GetMetrics(conn); connMetrics != nil {
		fmt.Println("\n=== CLIENT METRICS REPORT ===")
		fmt.Printf("Write Transactions:  %d\n", connMetrics.GetWriteTransactionCount())
		fmt.Printf("Read Transactions:   %d\n", connMetrics.GetReadTransactionCount())
		fmt.Printf("List Transactions:   %d\n", connMetrics.GetListTransactionCount())
		fmt.Printf("Delete Transactions: %d\n", connMetrics.GetDeleteTransactionCount())
		fmt.Printf("Bytes Sent:          %d\n", connMetrics.GetBytesSent())
		fmt.Printf("Bytes Received:      %d\n", connMetrics.GetBytesReceived())
		fmt.Println("==============================")
	}

	// Wait a bit for server to finish its logs if needed
	time.Sleep(1 * time.Second)
}
