package aznet

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
)

type rotationHTTP func(*http.Request) (*http.Response, error)

func (f rotationHTTP) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestBlobRotationCreationFailurePreservesTX(t *testing.T) {
	calls := 0
	client, err := container.NewClientWithNoCredential("https://rotation.invalid/session", &container.ClientOptions{ClientOptions: azcore.ClientOptions{
		Retry: policy.RetryOptions{MaxRetries: -1},
		Transport: rotationHTTP(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.Path != "/session/req-1" {
				t.Errorf("wrong target: %s", r.URL.Path)
			}
			status := http.StatusCreated
			body := ""
			if calls == 1 {
				status = http.StatusForbidden
				body = `<Error><Code>AuthorizationFailure</Code></Error>`
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/xml"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	tr := &blobTransport{containerClient: client, cfg: applyConfig(nil), isInitiator: true, txBlob: "req-0", txOffset: 123, blocksWritten: MaxBlocksPerBlob - 10}
	if err := tr.RotateTX(context.Background()); err == nil {
		t.Fatal("expected creation failure")
	}
	if tr.txSeq != 0 || tr.txBlob != "req-0" || tr.txOffset != 123 || tr.blocksWritten != MaxBlocksPerBlob-10 {
		t.Fatal("failed creation changed TX identity or offsets")
	}
	if err := tr.RotateTX(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tr.txSeq != 1 || tr.txBlob != "req-1" || tr.txOffset != 0 || tr.blocksWritten != 0 {
		t.Fatal("successful creation did not commit TX")
	}
}
