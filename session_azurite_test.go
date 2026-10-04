package aznet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/google/uuid"
)

func TestAzuriteBatchedSessionLifecycle(t *testing.T) {
	if os.Getenv("AZNET_AZURITE") != "1" {
		t.Skip("set AZNET_AZURITE=1")
	}
	const key = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
	for i, network := range []string{"azblob", "azqueue", "aztable"} {
		t.Run(network, func(t *testing.T) {
			u, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d/devstoreaccount1", 10000+i))
			u.User = url.UserPassword("devstoreaccount1", key)
			suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			opts := []Option{WithContext(ctx), WithEndpoints("h"+suffix, "t"+suffix), WithPing(0), WithDataPoll(5 * time.Millisecond), WithAcceptPoll(5 * time.Millisecond)}
			listener, err := Listen(network, u.String(), opts...)
			if err != nil {
				t.Fatal(err)
			}
			l := listener.(*Listener)
			defer func() {
				l.Close()
				cleanupCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
				defer done()
				if err := l.CleanupBootstrap(cleanupCtx); err != nil {
					t.Error(err)
				}
			}()
			address, err := l.ConnectionString()
			if err != nil {
				t.Fatal(err)
			}
			const count = 8
			clients := make(chan net.Conn, count)
			errs := make(chan error, count)
			for j := 0; j < count; j++ {
				go func() {
					c, err := Dial(network, address, opts...)
					if err != nil {
						errs <- err
						return
					}
					clients <- c
				}()
			}
			// Let requests accumulate so GetHandshakes returns a real batch.
			time.Sleep(100 * time.Millisecond)
			var accepted []*Conn
			for j := 0; j < count; j++ {
				c, err := l.Accept()
				if err != nil {
					t.Fatal(err)
				}
				accepted = append(accepted, c.(*Conn))
			}
			for j := 0; j < count; j++ {
				select {
				case c := <-clients:
					defer c.Close()
					if _, err := c.Write([]byte("ready")); err != nil {
						t.Fatal(err)
					}
				case err := <-errs:
					t.Fatal(err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			for _, c := range accepted {
				buf := make([]byte, 5)
				if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ready" {
					t.Fatal(string(buf), err)
				}
			}
			// No janitor tick has elapsed. Close must reclaim all accepted sessions.
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			for _, c := range accepted {
				var checks []error
				switch d := l.driver.(*metricsDriver).Driver.(type) {
				case *blobDriver:
					_, err := d.client.NewContainerClient(c.id).GetProperties(ctx, nil)
					checks = append(checks, err)
				case *queueDriver:
					for _, prefix := range []string{d.cfg.reqPrefix, d.cfg.resPrefix} {
						_, err := d.client.NewQueueClient(prefix+"-"+c.id).GetProperties(ctx, nil)
						checks = append(checks, err)
					}
				case *tableDriver:
					for _, prefix := range []string{d.cfg.reqPrefix, d.cfg.resPrefix} {
						_, err := d.client.NewClient(prefix+strings.ReplaceAll(c.id, "-", "")).GetAccessPolicy(ctx, nil)
						checks = append(checks, err)
					}
				}
				for _, err := range checks {
					var response *azcore.ResponseError
					if !errors.As(err, &response) || response.StatusCode != 404 {
						t.Fatalf("session resource remains: %v", err)
					}
				}
			}
			// Shared bootstrap remains usable by a later listener generation.
			if _, _, err := l.driver.CreateBootstrapTokens(); err != nil {
				t.Fatal(err)
			}
			if _, err := l.driver.GetHandshakes(ctx); err != nil {
				t.Fatal("bootstrap was removed", err)
			}
		})
	}
}
