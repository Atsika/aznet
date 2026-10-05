package aznet

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAzuriteTableReclamation(t *testing.T) {
	if os.Getenv("AZNET_AZURITE") != "1" {
		t.Skip("set AZNET_AZURITE=1")
	}
	const key = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg := applyConfig([]Option{WithContext(ctx), WithPing(0)})
	defer cfg.cancel()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	cfg.handshakeEndpoint, cfg.tokenEndpoint = "h"+suffix, "t"+suffix
	u, _ := url.Parse("http://127.0.0.1:10002/devstoreaccount1")
	u.User = url.UserPassword("devstoreaccount1", key)
	driver, err := (&tableFactory{}).NewDriver(NewEndpoint(u), cfg)
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
	client, err := driver.NewTransport(ctx, id, tokens, true)
	if err != nil {
		t.Fatal(err)
	}
	server, err := driver.NewTransport(ctx, id, tokens, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]Transport{{client, server}, {server, client}} {
		tx, rx := pair[0].(*tableTransport), pair[1].(*tableTransport)
		for seq := range tableCleanupRows + 1 {
			payload := bytes.Repeat([]byte{byte(seq)}, 128)
			if err := tx.WriteRaw(ctx, uint64(seq), bytes.NewReader(payload)); err != nil {
				t.Fatal(err)
			}
			body, err := rx.ReadRaw(ctx)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(body)
			body.Close()
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatal("byte identity", err)
			}
		}
		if _, err := rx.ReadRaw(ctx); !errors.Is(err, ErrNoData) {
			t.Fatal(err)
		}
		if err := tx.WriteRaw(ctx, 0, bytes.NewReader(bytes.Repeat([]byte{0}, 128))); err != nil {
			t.Fatal(err)
		}
		pager := rx.rxClient.NewListEntitiesPager(nil)
		var retained int
		for pager.More() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				t.Fatal(err)
			}
			retained += len(page.Entities)
		}
		if retained != 1 {
			t.Fatalf("retained %d rows; want last retry receipt only", retained)
		}
	}
}
