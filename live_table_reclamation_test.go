package aznet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/data/aztables"
	"github.com/google/uuid"
)

// liveTableHTTP injects client-side response loss only after the real service
// responds. SDK retries are disabled so each uncertain outcome reaches aznet.
// Tests use it serially and never log credential-bearing URLs or SDK errors.
type liveTableHTTP struct {
	client                                       *http.Client
	dropWrite, dropBatch, dropDelete, failDelete bool
	retryBody                                    []byte
	retryVerified                                bool
	batches                                      int
}

func (h *liveTableHTTP) Do(r *http.Request) (*http.Response, error) {
	batch := strings.HasSuffix(r.URL.Path, "/$batch")
	write := r.Method == http.MethodPost && !batch
	if write {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if h.retryBody != nil {
			if !bytes.Equal(body, h.retryBody) {
				return nil, errors.New("live retry entity changed")
			}
			h.retryVerified = true
			h.retryBody = nil
		}
		if h.dropWrite {
			h.retryBody = bytes.Clone(body)
			h.retryVerified = false
		}
	}
	if r.Method == http.MethodDelete && h.failDelete {
		h.failDelete = false
		return nil, io.ErrUnexpectedEOF
	}
	if batch {
		h.batches++
	}
	resp, err := h.client.Do(r)
	if err != nil {
		return resp, err
	}
	drop := false
	if write && h.dropWrite && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		h.dropWrite = false
		drop = true
	}
	if batch && h.dropBatch && resp.StatusCode == 202 {
		h.dropBatch = false
		drop = true
	}
	if r.Method == http.MethodDelete && h.dropDelete && resp.StatusCode == 204 {
		h.dropDelete = false
		drop = true
	}
	if drop {
		_, err := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		return nil, io.ErrUnexpectedEOF
	}
	return resp, nil
}

// TestLiveTableReclamation creates/deletes only UUID-named session tables.
// AZNET_LIVE_CONFIG is the explicit opt-in used by the existing live tests.
func TestLiveTableReclamation(t *testing.T) {
	path := os.Getenv("AZNET_LIVE_CONFIG")
	if path == "" {
		t.Skip("set AZNET_LIVE_CONFIG to enable live Azure resource operations")
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
	var ep *Endpoint
	for _, c := range config.Listeners {
		u, err := url.Parse(c.Address)
		if err == nil && c.Driver == "aztable" && u.Scheme == "https" && strings.HasSuffix(u.Hostname(), ".table.core.windows.net") && c.Account != "" && c.Key != "" {
			u.User = url.UserPassword(c.Account, c.Key)
			u.Path = ""
			u.RawQuery = ""
			u.Fragment = ""
			ep = NewEndpoint(u)
			break
		}
	}
	if ep == nil {
		t.Fatal("no live Table account-key configuration")
	}
	check := func(t *testing.T, step string, err error) {
		t.Helper()
		if err == nil {
			return
		}
		var response *azcore.ResponseError
		if errors.As(err, &response) {
			t.Fatalf("%s: status=%d code=%s", step, response.StatusCode, response.ErrorCode)
		}
		t.Fatalf("%s: error type %T", step, err)
	}
	for _, sasReceiver := range []bool{true, false} {
		t.Run(fmt.Sprintf("sas_receiver_%t", sasReceiver), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			cfg := applyConfig([]Option{WithContext(ctx), WithPing(0)})
			defer cfg.cancel()
			svc, err := newTableClient(ep, nil)
			check(t, "service client", err)
			driver := &tableDriver{client: svc, ep: ep, cfg: cfg}
			id := uuid.NewString()
			sid := strings.ReplaceAll(id, "-", "")
			reqName, resName := cfg.reqPrefix+sid, cfg.resPrefix+sid
			t.Logf("isolated tables: %s %s", reqName, resName)
			// Register before creation, including partial CreateSession failure. Use a
			// separate context so test cancellation cannot prevent residual cleanup.
			t.Cleanup(func() {
				cleanup, done := context.WithTimeout(context.Background(), 45*time.Second)
				defer done()
				err := driver.CleanupSession(cleanup, id)
				if err != nil {
					t.Error("session cleanup failed; test table names logged above")
					return
				}
				for _, name := range []string{reqName, resName} {
					// Query the table catalog, not merely an already-reclaimed
					// entity: an entity 404 alone cannot prove table deletion.
					filter := "TableName eq '" + name + "'"
					pager := svc.NewListTablesPager(&aztables.ListTablesOptions{Filter: &filter})
					for pager.More() {
						page, err := pager.NextPage(cleanup)
						check(t, "residual table catalog query", err)
						if len(page.Tables) != 0 {
							t.Fatal("residual session table remains in catalog")
						}
					}
					_, err := svc.NewClient(name).GetEntity(cleanup, "data", formatRowKey(0), nil)
					var response *azcore.ResponseError
					if !errors.As(err, &response) || response.StatusCode != 404 {
						check(t, "residual entity query", err)
						t.Fatal("expected residual entity 404")
					}
					t.Logf("removed table %s: catalog matches=0; entity status=%d code=%s", name, response.StatusCode, response.ErrorCode)
				}
				t.Log("session cleanup verified: both tables absent from catalog and entity requests return 404")
			})
			tokens, err := driver.CreateSession(ctx, id)
			check(t, "create session", err)
			client, err := driver.NewTransport(ctx, id, tokens, true)
			check(t, "initiator transport", err)
			server, err := driver.NewTransport(ctx, id, tokens, false)
			check(t, "responder transport", err)
			tx, rx := server.(*tableTransport), client.(*tableTransport)
			name, token := resName, tokens.Res
			if !sasReceiver {
				tx, rx = client.(*tableTransport), server.(*tableTransport)
				name, token = reqName, tokens.Req
			}
			httpClient := &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
			defer httpClient.CloseIdleConnections()
			txHTTP, rxHTTP := &liveTableHTTP{client: httpClient}, &liveTableHTTP{client: httpClient}
			options := func(h *liveTableHTTP) *aztables.ClientOptions {
				return &aztables.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}, Transport: h}}
			}
			cred, err := aztables.NewSharedKeyCredential(ep.Account, ep.Key)
			check(t, "credential", err)
			if sasReceiver {
				tx.txClient, err = aztables.NewClientWithSharedKey(ep.JoinURL(name, ""), cred, options(txHTTP))
				check(t, "writer", err)
				rx.rxClient, err = aztables.NewClientWithNoCredential(ep.JoinURL(name, token), options(rxHTTP))
				check(t, "SAS reader", err)
			} else {
				tx.txClient, err = aztables.NewClientWithNoCredential(ep.JoinURL(name, token), options(txHTTP))
				check(t, "SAS writer", err)
				rx.rxClient, err = aztables.NewClientWithSharedKey(ep.JoinURL(name, ""), cred, options(rxHTTP))
				check(t, "reader", err)
			}
			admin := svc.NewClient(name)
			count := func(want int) {
				t.Helper()
				pager := admin.NewListEntitiesPager(nil)
				n := 0
				for pager.More() {
					page, err := pager.NextPage(ctx)
					check(t, "independent retained-row query", err)
					n += len(page.Entities)
				}
				if n != want {
					t.Fatalf("retained rows=%d want=%d", n, want)
				}
			}
			a, b := reviewNoise(t)
			ciphertext := make([][]byte, 3*tableCleanupRows+1)
			plaintext := make([][]byte, len(ciphertext))
			for seq := range ciphertext {
				plaintext[seq] = []byte(fmt.Sprintf("live encrypted row %09d", seq))
				ciphertext[seq], err = a.SealData(nil, plaintext[seq])
				check(t, "seal", err)
			}
			write := func(seq int) {
				t.Helper()
				check(t, "write row", tx.WriteRaw(ctx, uint64(seq), bytes.NewReader(ciphertext[seq])))
			}
			consume := func(end int) {
				t.Helper()
				for rx.rxSeq < end {
					start := rx.rxSeq
					body, err := rx.ReadRaw(ctx)
					check(t, "fetch", err)
					raw, err := io.ReadAll(body)
					body.Close()
					check(t, "consume", err)
					var expected []byte
					for seq := start; seq < rx.rxSeq; seq++ {
						expected = append(expected, ciphertext[seq]...)
					}
					if !bytes.Equal(raw, expected) {
						t.Fatal("ciphertext byte identity/order changed")
					}
					for seq := start; seq < rx.rxSeq; seq++ {
						plain, rest, err := b.UnsealData(nil, raw, MaxTableEntitySize)
						check(t, "decrypt", err)
						if !bytes.Equal(plain, plaintext[seq]) {
							t.Fatal("plaintext ordering changed")
						}
						raw = rest
					}
					if len(raw) != 0 {
						t.Fatal("encrypted frame remainder")
					}
				}
			}
			empty := func() {
				t.Helper()
				_, err := rx.ReadRaw(ctx)
				if !errors.Is(err, ErrNoData) {
					check(t, "cleanup/empty poll", err)
					t.Fatal("expected empty poll")
				}
			}
			uncertain := func(seq int) {
				t.Helper()
				txHTTP.dropWrite = true
				err := tx.WriteRaw(ctx, uint64(seq), bytes.NewReader(ciphertext[seq]))
				if !errors.Is(err, io.ErrUnexpectedEOF) || txHTTP.dropWrite {
					t.Fatal("real successful write response was not dropped")
				}
			}
			uncertain(0)
			write(0)
			if !txHTTP.retryVerified {
				t.Fatal("pre-consumption retry not verified")
			}
			for seq := 1; seq < tableCleanupRows; seq++ {
				write(seq)
			}
			uncertain(tableCleanupRows)
			count(tableCleanupRows + 1)
			consume(tableCleanupRows + 1)
			empty() // Strict: SAS batch must succeed; no emulator exception here.
			if rxHTTP.batches != 1 || rx.reclaimSeq != tableCleanupRows {
				t.Fatal("full batch did not commit")
			}
			count(1)
			write(tableCleanupRows)
			if !txHTTP.retryVerified {
				t.Fatal("post-consumption retry not verified")
			}
			write(0)
			count(1)
			t.Log("100-row batch succeeded; ciphertext-identical retries before/after consumption; latest receipt retained; old retry did not recreate row")

			for seq := tableCleanupRows + 1; seq <= 2*tableCleanupRows; seq++ {
				write(seq)
			}
			consume(2*tableCleanupRows + 1)
			rxHTTP.dropBatch = true
			_, err = rx.ReadRaw(ctx)
			if !errors.Is(err, io.ErrUnexpectedEOF) || rxHTTP.dropBatch || rx.reclaimSeq != tableCleanupRows {
				t.Fatal("uncertain batch did not retain cursor")
			}
			count(1) // Independently prove the real batch committed before response loss.
			rxHTTP.failDelete = true
			_, err = rx.ReadRaw(ctx)
			if !errors.Is(err, io.ErrUnexpectedEOF) || rx.reclaimSeq != tableCleanupRows {
				t.Fatal("reconciliation failure advanced cursor")
			}
			empty()
			count(1)
			if rx.reclaimSeq != 2*tableCleanupRows {
				t.Fatal("uncertain batch reconciliation incomplete")
			}
			t.Log("real batch committed with response withheld; failed reconciliation preserved cursor; 100 individual 404s reconciled safely")

			for seq := 2*tableCleanupRows + 1; seq <= 3*tableCleanupRows; seq++ {
				write(seq)
			}
			consume(3*tableCleanupRows + 1)
			_, err = admin.DeleteEntity(ctx, "data", formatRowKey(2*tableCleanupRows+50), nil)
			check(t, "remove middle row", err)
			count(tableCleanupRows)
			_, err = rx.ReadRaw(ctx)
			if err == nil || errors.Is(err, ErrNoData) || rx.reclaimSeq != 2*tableCleanupRows || !rx.reclaimSingles {
				t.Fatal("missing-row batch failure was hidden")
			}
			count(tableCleanupRows) // Missing entity must roll back every other delete.
			rxHTTP.dropDelete = true
			_, err = rx.ReadRaw(ctx)
			if !errors.Is(err, io.ErrUnexpectedEOF) || rxHTTP.dropDelete || rx.reclaimSeq != 2*tableCleanupRows {
				t.Fatal("uncertain individual delete advanced cursor")
			}
			count(tableCleanupRows - 1)
			empty()
			count(1)
			if rx.reclaimSeq != 3*tableCleanupRows || rxHTTP.batches != 3 {
				t.Fatal("final cleanup progress")
			}
			t.Log("missing-row transaction rolled back; lost individual delete response reconciled; final retained rows=1 (newest receipt)")
		})
	}
}
