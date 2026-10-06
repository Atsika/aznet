package aznet

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestAzuriteConnectionContract exercises the same encrypted connection contract
// through every registered storage adapter. Only the local emulator is used.
func TestAzuriteConnectionContract(t *testing.T) {
	if os.Getenv("AZNET_AZURITE") != "1" {
		t.Skip("set AZNET_AZURITE=1 with Azurite on localhost:10000-10002")
	}
	// Azurite's public development credential, also used by the examples.
	const key = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
	for i, scheme := range []string{"azblob", "azqueue", "aztable"} {
		t.Run(scheme, func(t *testing.T) {
			cfg := applyConfig([]Option{WithPing(0)})
			defer cfg.cancel()
			suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
			cfg.handshakeEndpoint = "h" + suffix
			cfg.tokenEndpoint = "t" + suffix
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			u := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", 10000+i), Path: "/devstoreaccount1", User: url.UserPassword("devstoreaccount1", key)}
			factory, ok := lookupFactory(scheme)
			if !ok {
				t.Fatal("driver not registered")
			}
			driver, err := factory.NewDriver(NewEndpoint(u), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer driver.CleanupBootstrap(ctx)
			id := uuid.NewString()
			tokens, err := driver.CreateSession(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			defer driver.CleanupSession(ctx, id)
			server, err := driver.NewTransport(ctx, id, tokens, false)
			if err != nil {
				t.Fatal(err)
			}
			client, err := driver.NewTransport(ctx, id, tokens, true)
			if err != nil {
				server.Close()
				t.Fatal(err)
			}
			a, b := reviewNoise(t)
			ac, ax := context.WithCancel(ctx)
			bc, bx := context.WithCancel(ctx)
			sender := newConn(ac, ax, client, cfg, a, nil, id)
			receiver := newConn(bc, bx, server, cfg, b, nil, id)
			defer sender.Close()
			defer receiver.Close()
			sender.SetDeadline(time.Now().Add(20 * time.Second))
			receiver.SetDeadline(time.Now().Add(20 * time.Second))
			payload := bytes.Repeat([]byte("aznet"), sender.MTU()/5+17)
			if n, err := sender.Write(payload); n != len(payload) || err != nil {
				t.Fatal(n, err)
			}
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(receiver, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(payload, got) {
				t.Fatal("forward payload differs")
			}
			// Force a real Blob resource rollover at a frame boundary without 50k writes.
			if blob, ok := client.(*blobTransport); ok {
				blob.txMu.Lock()
				blob.blocksWritten = MaxBlocksPerBlob - 10
				blob.txMu.Unlock()
			}
			if _, err := sender.Write([]byte("tail")); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(receiver, got[:4]); err != nil || string(got[:4]) != "tail" {
				t.Fatal("rollover/tail", err)
			}
			if _, err := receiver.Write([]byte("reply")); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(sender, got[:5]); err != nil || string(got[:5]) != "reply" {
				t.Fatal("reverse payload", err)
			}
			if err := sender.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			if _, err := receiver.Read(got[:1]); err != io.EOF {
				t.Fatalf("FIN: %v", err)
			}
		})
	}
}
