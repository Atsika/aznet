package aznet

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

// Read and write own distinct append blobs. A stalled HTTP operation in one
// direction must not prevent the other from reaching Azure's SDK transport.
func TestBlobDirectionsProgressIndependently(t *testing.T) {
	for _, stalled := range []string{http.MethodGet, http.MethodPut} {
		t.Run(stalled, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			other := make(chan struct{})
			tr := newReadTestBlob(t, func(r *http.Request) (*http.Response, error) {
				if r.Method == stalled {
					close(entered)
					<-release
				} else {
					close(other)
				}
				if r.Method == http.MethodGet {
					return blobReadResponse(r, io.NopCloser(bytes.NewReader([]byte{42})), 1), nil
				}
				return &http.Response{StatusCode: http.StatusCreated, Header: make(http.Header), Body: http.NoBody, Request: r}, nil
			})
			tr.txBlob = "req-0"
			read := func() error {
				body, err := tr.ReadRaw(context.Background())
				if err == nil {
					_, err = io.ReadAll(body)
					body.Close()
				}
				return err
			}
			write := func() error { return tr.WriteRaw(context.Background(), 1, bytes.NewReader([]byte{7})) }
			first, second := read, write
			if stalled == http.MethodPut {
				first, second = write, read
			}
			done := make(chan error, 2)
			go func() { done <- first() }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("first operation never reached SDK")
			}
			go func() { done <- second() }()
			select {
			case <-other:
			case <-time.After(200 * time.Millisecond):
				t.Error("opposite direction blocked behind stalled SDK operation")
			}
			close(release)
			for i := 0; i < 2; i++ {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
