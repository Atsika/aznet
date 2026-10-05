package aznet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/google/uuid"
)

// TestLiveBootstrapDeletionWindow is opt-in: AZNET_LIVE_CONFIG names a local
// ProxyBlob-format configuration containing Azure account credentials. It creates
// and deletes only UUID-named bootstrap resources, never configured endpoints.
func TestLiveBootstrapDeletionWindow(t *testing.T) {
	path := os.Getenv("AZNET_LIVE_CONFIG")
	if path == "" {
		t.Skip("set AZNET_LIVE_CONFIG to explicitly enable live Azure resource operations")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read live configuration")
	}
	var config struct {
		Listeners []struct {
			Driver  string `json:"driver"`
			Address string `json:"address"`
			Account string `json:"storage_account"`
			Key     string `json:"storage_account_key"`
		} `json:"listeners"`
	}
	if json.Unmarshal(raw, &config) != nil {
		t.Fatal("invalid live configuration")
	}
	for _, network := range []string{"azblob", "azqueue", "aztable"} {
		t.Run(network, func(t *testing.T) {
			var address string
			for _, c := range config.Listeners {
				u, err := url.Parse(c.Address)
				if err == nil && c.Driver == network && u.Scheme == "https" && strings.HasSuffix(u.Hostname(), ".core.windows.net") && c.Account != "" && c.Key != "" {
					u.User = url.UserPassword(c.Account, c.Key)
					u.RawQuery = ""
					u.Path = ""
					address = u.String()
					break
				}
			}
			if address == "" {
				t.Fatal("no live account-key configuration for driver")
			}
			prefix := "aznetreview" + strings.ReplaceAll(uuid.NewString(), "-", "")
			handshake, token := prefix+"h", prefix+"t"
			t.Logf("isolated bootstrap resources: %s, %s", handshake, token)
			describe := func(err error) string {
				var response *azcore.ResponseError
				if errors.As(err, &response) {
					return fmt.Sprintf("status=%d code=%s", response.StatusCode, response.ErrorCode)
				}
				if err == nil {
					return "success"
				}
				// SDK errors may contain credential-bearing request URLs. Never print them.
				return fmt.Sprintf("error type %T", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			listen := func() (net.Listener, error) {
				return Listen(network, address, WithContext(ctx), WithEndpoints(handshake, token))
			}
			// Independent cleanup also covers partially successful initialization.
			u, _ := url.Parse(address)
			ep := NewEndpoint(u)
			remove := func(ctx context.Context, name string) error {
				switch network {
				case "azblob":
					c, err := newBlobClient(ep, nil)
					if err != nil {
						return err
					}
					_, err = c.NewContainerClient(name).Delete(ctx, nil)
					return err
				case "azqueue":
					c, err := newQueueClient(ep, nil)
					if err != nil {
						return err
					}
					_, err = c.NewQueueClient(name).Delete(ctx, nil)
					return err
				default:
					c, err := newTableClient(ep, nil)
					if err != nil {
						return err
					}
					_, err = c.DeleteTable(ctx, name, nil)
					return err
				}
			}
			t.Cleanup(func() {
				cleanupCtx, done := context.WithTimeout(context.Background(), 30*time.Second)
				defer done()
				for _, name := range []string{handshake, token} {
					err := remove(cleanupCtx, name)
					var response *azcore.ResponseError
					if err != nil && !(errors.As(err, &response) && response.StatusCode == 404) {
						t.Errorf("cleanup %s: %s", name, describe(err))
					}
				}
			})
			listener, err := listen()
			if err != nil {
				t.Fatalf("initial Listen: %s", describe(err))
			}
			if err := listener.Close(); err != nil {
				t.Fatalf("initial Close: %s", describe(err))
			}
			if err := listener.(*Listener).CleanupBootstrap(ctx); err != nil {
				t.Fatalf("explicit bootstrap cleanup: %s", describe(err))
			}
			start := time.Now()
			deleting := 0
			for {
				listener, err = listen()
				if err == nil {
					if err := listener.Close(); err != nil {
						t.Errorf("final Close: %s", describe(err))
					}
					if deleting == 0 {
						t.Fatal("no deletion-window response observed; live classification gate not established")
					}
					t.Logf("name reuse succeeded after %s; %d deletion responses validated", time.Since(start).Round(time.Millisecond), deleting)
					break
				}
				wrapped := fmt.Errorf("caller: %w", err)
				var response *azcore.ResponseError
				wantCode := map[string]string{"azblob": "ContainerBeingDeleted", "azqueue": "QueueBeingDeleted", "aztable": "TableBeingDeleted"}[network]
				if !errors.Is(wrapped, ErrResourceBeingDeleted) || !errors.As(wrapped, &response) || response.StatusCode != 409 || response.ErrorCode != wantCode || response.RawResponse == nil || !errors.Is(wrapped, response) {
					t.Fatalf("restart classification: %s; sentinel=%t", describe(err), errors.Is(wrapped, ErrResourceBeingDeleted))
				}
				if !strings.Contains(err.Error(), handshake) && !strings.Contains(err.Error(), token) {
					t.Fatal("resource context missing")
				}
				deleting++
				if deleting == 1 {
					t.Logf("immediate restart: %s; sentinel and SDK chain preserved", describe(err))
				}
				select {
				case <-ctx.Done():
					t.Fatal("name reuse not observed within three minutes")
				case <-time.After(5 * time.Second):
				}
			}
		})
	}
}
