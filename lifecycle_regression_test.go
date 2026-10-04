package aznet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"io"
	"os"
	"testing"
	"time"
)

type cancelBody struct {
	*bytes.Reader
	cancel func()
}

func (b *cancelBody) Close() error { return nil }
func (b *cancelBody) Read(p []byte) (int, error) {
	n, e := b.Reader.Read(p)
	if e == io.EOF {
		b.cancel()
	}
	return n, e
}
func TestReadRecoversBufferedCiphertextAfterDeadline(t *testing.T) {
	sender, receiver := reviewNoise(t)
	var plain bytes.Buffer
	BuildFrame(&plain, Frame{Type: MsgTypeData, Payload: []byte("hello")})
	sealed, e := sender.SealData(nil, plain.Bytes())
	if e != nil {
		t.Fatal(e)
	}
	var c *Conn
	calls := 0
	tr := &reviewTransport{read: func(ctx context.Context) (io.ReadCloser, error) {
		calls++
		if calls == 1 {
			return &cancelBody{bytes.NewReader(sealed), func() { c.SetReadDeadline(time.Now().Add(-time.Second)) }}, nil
		}
		return nil, ErrNoData
	}}
	c = reviewConn(tr, receiver)
	defer c.Close()
	b := make([]byte, 5)
	_, err := c.Read(b)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(40 * time.Millisecond))
	n, err := c.Read(b)
	if n != 5 || string(b) != "hello" || err != nil {
		t.Fatalf("recovery n=%d bytes=%q error=%v retainedCiphertext=%d", n, b, err, c.bufs.Noise.Len())
	}
}

func TestCleanupPreservesJoinedFailures(t *testing.T) {
	missing := &azcore.ResponseError{StatusCode: 404}
	denied := &azcore.ResponseError{StatusCode: 403}
	for _, err := range []error{errors.Join(missing, denied), fmt.Errorf("custom driver: %w", errors.Join(missing, denied)), errors.Join(denied, missing)} {
		got := cleanup("session", func(context.Context) error { return err })
		if !errors.Is(got, denied) {
			t.Fatalf("hidden deletion failure: %v", got)
		}
	}
	if err := cleanup("already removed", func(context.Context) error { return fmt.Errorf("driver: %w", errors.Join(missing, missing)) }); err != nil {
		t.Fatal(err)
	}
}

type failingCloseTransport struct {
	reviewTransport
	calls   int
	failure error
}

func (t *failingCloseTransport) Close() error {
	t.calls++
	return t.failure
}
func TestRollbackDoesNotRetryTransportClose(t *testing.T) {
	failure := &azcore.ResponseError{StatusCode: 503}
	transport := &failingCloseTransport{failure: failure}
	driver := &sessionDriver{transport: func(context.Context) (Transport, error) { return transport, errors.New("partial acquisition") }}
	listener := sessionListener(t, driver, 1)
	if _, err := listener.Accept(); !errors.Is(err, failure) {
		t.Fatalf("lost close failure: %v", err)
	}
	if transport.calls != 1 {
		t.Fatalf("Close called %d times for one acquisition", transport.calls)
	}
}
