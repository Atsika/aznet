package aznet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

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

func TestBlobRangeUsesRemainingReceiveAllowance(t *testing.T) {
	for _, metrics := range []bool{false, true} {
		t.Run(fmt.Sprint(metrics), func(t *testing.T) {
			a, b := reviewNoise(t)
			var ciphertext []byte
			for range 3 {
				var frame bytes.Buffer
				BuildFrame(&frame, Frame{Type: MsgTypeData, Payload: bytes.Repeat([]byte{42}, 875)})
				raw, err := a.SealData(nil, frame.Bytes())
				if err != nil {
					t.Fatal(err)
				}
				ciphertext = append(ciphertext, raw...)
			}
			var c *Conn
			tr := newReadTestBlob(t, func(r *http.Request) (*http.Response, error) {
				var start, end int
				if _, err := fmt.Sscanf(blobRequestRange(r), "bytes=%d-%d", &start, &end); err != nil {
					return nil, err
				}
				if end-start+1 > 1000-c.bufs.Noise.Len() {
					t.Error("range exceeds remaining allowance")
				}
				end = min(end+1, len(ciphertext))
				return blobReadResponse(r, io.NopCloser(bytes.NewReader(ciphertext[start:end])), end-start), nil
			})
			cfg := applyConfig([]Option{WithPing(0), WithBufferLimits(BufferLimits{Pending: 1000, Retry: 1000})})
			tr.cfg = cfg
			var transport Transport = tr
			if metrics {
				transport = newMetricsTransport(tr, NewDefaultMetrics())
			}
			c = newConn(cfg.ctx, cfg.cancel, transport, cfg, b, nil, "bounded")
			defer cfg.cancel()
			got := make([]byte, 3*875)
			if _, err := io.ReadFull(c, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, bytes.Repeat([]byte{42}, len(got))) {
				t.Fatal("bounded ranges changed byte identity or order")
			}
		})
	}
}

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

// Return bytes and an error together, as a response interrupted mid-ciphertext can.
type interruptedBlobBody struct {
	data   []byte
	err    error
	closed bool
}

func (b *interruptedBlobBody) Read(p []byte) (int, error) {
	n := copy(p, b.data)
	b.data = b.data[n:]
	if len(b.data) == 0 {
		return n, b.err
	}
	return n, nil
}

func (b *interruptedBlobBody) Close() error { b.closed = true; return nil }

func newReadTestBlob(t *testing.T, send rotationHTTP) *blobTransport {
	t.Helper()
	client, err := container.NewClientWithNoCredential("https://read.invalid/session", &container.ClientOptions{ClientOptions: azcore.ClientOptions{
		Retry: policy.RetryOptions{MaxRetries: -1}, Transport: send,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return &blobTransport{containerClient: client, rxBlob: "res-0"}
}

func blobReadResponse(r *http.Request, body io.ReadCloser, length int) *http.Response {
	return &http.Response{StatusCode: http.StatusPartialContent, Header: http.Header{"Content-Length": {strconv.Itoa(length)}}, ContentLength: int64(length), Body: body, Request: r}
}

func TestBlobReadInterruptedCiphertextRetry(t *testing.T) {
	for _, bodyErr := range []error{io.ErrUnexpectedEOF, context.Canceled, io.EOF} {
		t.Run(bodyErr.Error(), func(t *testing.T) {
			senderNoise, receiverNoise := reviewNoise(t)
			var ciphertext []byte
			sender := reviewConn(&reviewTransport{write: func(_ context.Context, _ uint64, r io.ReadSeeker) error {
				data, err := io.ReadAll(r)
				ciphertext = append(ciphertext, data...)
				return err
			}}, senderNoise)
			defer sender.cancel()
			want := []byte("first encrypted message; second encrypted message")
			for _, data := range [][]byte{want[:25], want[25:]} {
				if _, err := sender.Write(data); err != nil {
					t.Fatal(err)
				}
			}
			const cut = 7 // Inside the first encrypted record, before it can be decrypted.
			broken := &interruptedBlobBody{data: append([]byte(nil), ciphertext[:cut]...), err: bodyErr}
			calls := 0
			tr := newReadTestBlob(t, func(r *http.Request) (*http.Response, error) {
				calls++
				switch calls {
				case 1:
					if got := blobRequestRange(r); got != "" && got != "bytes=0-4194303" {
						t.Errorf("initial range = %q", got)
					}
					return blobReadResponse(r, broken, len(ciphertext)), nil
				case 2:
					if got := blobRequestRange(r); got != "bytes=7-4194310" {
						t.Errorf("retry range = %q, want bytes=7-", got)
					}
					return blobReadResponse(r, io.NopCloser(bytes.NewReader(ciphertext[cut:])), len(ciphertext)-cut), nil
				default:
					return nil, fmt.Errorf("unexpected extra poll %d", calls)
				}
			})
			receiver := reviewConn(tr, receiverNoise)
			defer receiver.cancel()
			receiver.SetReadDeadline(time.Now().Add(5 * time.Second))
			got := make([]byte, len(want))
			if bodyErr != io.EOF {
				n, err := receiver.Read(got)
				if n != 0 || !errors.Is(err, bodyErr) {
					t.Fatalf("interrupted read = %d, %v", n, err)
				}
			}
			if _, err := io.ReadFull(receiver, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("payload = %q, want %q", got, want)
			}
			if calls != 2 || !broken.closed {
				t.Fatalf("calls=%d, interrupted body closed=%v", calls, broken.closed)
			}
		})
	}
}

func TestBlobReadEarlyCloseResumesConsumedBytes(t *testing.T) {
	const want = "opaque ciphertext in storage"
	calls := 0
	tr := newReadTestBlob(t, func(r *http.Request) (*http.Response, error) {
		offset := 0
		if calls > 0 {
			offset = 3
		}
		calls++
		if offset > 0 && blobRequestRange(r) != "bytes=3-4194306" {
			t.Errorf("resume range = %q", blobRequestRange(r))
		}
		return blobReadResponse(r, io.NopCloser(strings.NewReader(want[offset:])), len(want)-offset), nil
	})
	body, err := tr.ReadRaw(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	prefix := make([]byte, 3)
	if _, err := io.ReadFull(body, prefix); err != nil {
		t.Fatal(err)
	}
	body.Close()
	body, err = tr.ReadRaw(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	rest, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(append(prefix, rest...)) != want {
		t.Fatal("ciphertext changed on resume")
	}
}

// SDK requests have not passed through net/http's header canonicalization yet.
func blobRequestRange(r *http.Request) string {
	for key, values := range r.Header {
		if strings.EqualFold(key, "x-ms-range") && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func TestBlobReadEmptyPollingPreservesProgress(t *testing.T) {
	for _, emptyStatus := range []int{http.StatusOK, http.StatusNotFound, http.StatusRequestedRangeNotSatisfiable} {
		t.Run(strconv.Itoa(emptyStatus), func(t *testing.T) {
			calls := 0
			empty := &interruptedBlobBody{err: io.EOF}
			tr := newReadTestBlob(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if calls > 1 && blobRequestRange(r) != "bytes=5-4194308" {
					t.Errorf("range after poll = %q", blobRequestRange(r))
				}
				switch calls {
				case 1:
					return blobReadResponse(r, io.NopCloser(strings.NewReader("first")), 5), nil
				case 2:
					response := blobReadResponse(r, empty, 0)
					response.StatusCode = emptyStatus
					return response, nil
				case 3:
					return blobReadResponse(r, io.NopCloser(strings.NewReader("second")), 6), nil
				default:
					return nil, errors.New("unexpected poll")
				}
			})
			var got []byte
			for i := 0; i < 3; i++ {
				body, err := tr.ReadRaw(context.Background())
				if i == 1 {
					if body != nil || !errors.Is(err, ErrNoData) {
						t.Fatalf("empty poll = %v, %v", body, err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(body)
				body.Close()
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, data...)
			}
			if string(got) != "firstsecond" {
				t.Fatalf("read = %q", got)
			}
			if emptyStatus == http.StatusOK && !empty.closed {
				t.Fatal("empty body not closed")
			}
		})
	}
}
