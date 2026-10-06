// A one-connection demonstration. The server owns and removes its demo namespace.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/atsika/aznet"
)

const message = "hello aznet\n"

func main() {
	driver := flag.String("driver", "azblob", "azblob, azqueue or aztable")
	listen := flag.String("listen", "", "server endpoint; omit for client mode using AZNET_URL")
	namespace := flag.String("namespace", "aznetdemo", "exclusive demo namespace; use the same value on both sides")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	opts := []aznet.Option{aznet.WithContext(ctx), aznet.WithEndpoints(*namespace+"handshake", *namespace+"token")}
	var err error
	if *listen != "" {
		err = serve(ctx, *driver, *listen, opts)
	} else {
		err = request(*driver, os.Getenv("AZNET_URL"), opts)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, driver, endpoint string, opts []aznet.Option) (result error) {
	listener, err := aznet.Listen(driver, endpoint, opts...)
	if err != nil {
		return errors.New("listen failed; check endpoint and credentials")
	}
	l := listener.(*aznet.Listener)
	defer func() {
		// This demo owns its entire namespace. Ordinary shared listeners must not do this.
		closeErr := l.Close()
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cleanupErr := l.CleanupBootstrap(cleanup)
		if closeErr != nil || cleanupErr != nil {
			result = errors.Join(result, errors.New("cleanup incomplete; inspect the demo namespace"))
		}
	}()
	url, err := l.ConnectionString()
	if err != nil {
		return errors.New("credential issuance failed")
	}
	fmt.Println(url) // Bearer secret: share only with the intended client.
	fmt.Fprintln(os.Stderr, "Waiting for one client")
	conn, err := l.Accept()
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return errors.New("accept failed")
	}
	defer conn.Close()
	if err = conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return errors.New("deadline setup failed")
	}
	input := make([]byte, len(message))
	if _, err = io.ReadFull(conn, input); err != nil {
		return errors.New("request read failed")
	}
	if !bytes.Equal(input, []byte(message)) {
		return errors.New("unexpected request")
	}
	if _, err = conn.Write(input); err != nil {
		return errors.New("response write failed")
	}
	if err = conn.(*aznet.Conn).CloseWrite(); err != nil {
		return errors.New("response half-close failed")
	}
	// Wait for the application acknowledgement before deleting session storage.
	var ack [1]byte
	if _, err = io.ReadFull(conn, ack[:]); err != nil || ack[0] != 1 {
		return errors.New("response acknowledgement missing")
	}
	if _, err = conn.Read(ack[:]); err != io.EOF {
		return errors.New("client EOF missing")
	}
	fmt.Fprintln(os.Stderr, "Round trip acknowledged; cleaning up")
	return nil
}

func request(driver, url string, opts []aznet.Option) error {
	if url == "" {
		return errors.New("set AZNET_URL to the server's generated URL")
	}
	conn, err := aznet.Dial(driver, url, opts...)
	if err != nil {
		return errors.New("dial failed; check driver, namespace and fresh URL")
	}
	defer conn.Close()
	if err = conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return errors.New("deadline setup failed")
	}
	if _, err = conn.Write([]byte(message)); err != nil {
		return errors.New("request write failed")
	}
	response, err := io.ReadAll(io.LimitReader(conn, int64(len(message)+1)))
	if err != nil || !bytes.Equal(response, []byte(message)) {
		return errors.New("response or ordered EOF failed")
	}
	if _, err = conn.Write([]byte{1}); err != nil {
		return errors.New("acknowledgement failed")
	}
	if err = conn.(*aznet.Conn).CloseWrite(); err != nil {
		return errors.New("client half-close failed")
	}
	fmt.Print(string(response))
	return nil
}
