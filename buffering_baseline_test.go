package aznet

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/data/aztables"
)

func TestLargeWriteDoesNotQueueEntirePayload(t *testing.T) {
	a, _ := reviewNoise(t)
	var c *Conn
	c = reviewConn(&reviewTransport{write: func(context.Context, uint64, io.ReadSeeker) error {
		if c.bufs.Write.Len() > 8<<20 {
			return fmt.Errorf("queued %d bytes, exceeds 8 MiB", c.bufs.Write.Len())
		}
		return nil
	}}, a)
	defer c.cancel()
	payload := bytes.Repeat([]byte("x"), 9<<20)
	if n, err := c.Write(payload); err != nil || n != len(payload) {
		t.Fatal(n, err)
	}
}

func TestTableFetchDoesNotConsumeRows(t *testing.T) {
	client, err := aztables.NewClientWithNoCredential("https://table.invalid/session", &aztables.ClientOptions{ClientOptions: azcore.ClientOptions{
		Retry: policy.RetryOptions{MaxRetries: -1},
		Transport: rotationHTTP(func(r *http.Request) (*http.Response, error) {
			return tableReadResponse(r, 200, `{"value":[{"PartitionKey":"data","RowKey":"000000000","Data":"YWJj","Data@odata.type":"Edm.Binary"}]}`), nil
		}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	tr := &tableTransport{rxClient: client}
	body, err := tr.ReadRaw(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if tr.rxSeq != 0 {
		t.Fatalf("fetch advanced consumption to %d", tr.rxSeq)
	}
}
