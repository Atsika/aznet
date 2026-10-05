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
	"github.com/Azure/azure-sdk-for-go/sdk/data/aztables"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
)

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
					if got := blobRequestRange(r); got != "" && got != "bytes=0-" {
						t.Errorf("initial range = %q", got)
					}
					return blobReadResponse(r, broken, len(ciphertext)), nil
				case 2:
					if got := blobRequestRange(r); got != "bytes=7-" {
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
		if offset > 0 && blobRequestRange(r) != "bytes=3-" {
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

func TestTableReadErrorsRemainObservable(t *testing.T) {
	for _, tc := range []struct {
		name, code   string
		status       int
		transportErr error
	}{
		{name: "authorization", code: "AuthorizationFailure", status: 403},
		{name: "service", code: "InternalError", status: 500},
		{name: "missing_table", code: "TableNotFound", status: 404},
		{name: "canceled", transportErr: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client, err := aztables.NewClientWithNoCredential("https://read.invalid/session", &aztables.ClientOptions{ClientOptions: azcore.ClientOptions{
				Retry: policy.RetryOptions{MaxRetries: -1},
				Transport: rotationHTTP(func(r *http.Request) (*http.Response, error) {
					calls++
					if got := r.URL.Query().Get("$filter"); got != "PartitionKey eq 'data' and RowKey ge '000000000'" {
						t.Errorf("filter changed after error: %q", got)
					}
					if calls > 1 {
						return tableReadResponse(r, 200, `{"value":[]}`), nil
					}
					if tc.transportErr != nil {
						return nil, tc.transportErr
					}
					return tableReadResponse(r, tc.status, fmt.Sprintf(`{"odata.error":{"code":%q,"message":{"lang":"en-US","value":"injected failure"}}}`, tc.code)), nil
				}),
			}})
			if err != nil {
				t.Fatal(err)
			}
			tr := &tableTransport{rxClient: client}
			_, noise := reviewNoise(t)
			conn := reviewConn(tr, noise)
			defer conn.cancel()
			conn.SetReadDeadline(time.Now().Add(time.Second))
			n, err := conn.Read(make([]byte, 1))
			if n != 0 || err == nil || errors.Is(err, ErrNoData) {
				t.Fatalf("read = %d, %v", n, err)
			}
			if tc.transportErr != nil {
				if !errors.Is(err, tc.transportErr) {
					t.Fatalf("lost transport error: %v", err)
				}
			} else {
				var sdkErr *azcore.ResponseError
				if !errors.As(err, &sdkErr) || sdkErr.StatusCode != tc.status || sdkErr.ErrorCode != tc.code {
					t.Fatalf("lost service error: %v", err)
				}
				if sdkErr.RawResponse.Header.Get("x-ms-request-id") != "read-integrity" {
					t.Fatal("lost response details")
				}
			}
			if calls != 1 {
				t.Fatalf("error triggered %d requests", calls)
			}
			body, err := tr.ReadRaw(context.Background())
			if body != nil || !errors.Is(err, ErrNoData) {
				t.Fatalf("empty poll = %v, %v", body, err)
			}
		})
	}
}

func tableReadResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}, "X-Ms-Request-Id": {"read-integrity"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func TestTableReadEmptyAndGappedPollingPreservesOrder(t *testing.T) {
	// Literal SDK entities: base64 encodes "first", "second", and "third".
	const first = `{"PartitionKey":"data","RowKey":"000000000","Data":"Zmlyc3Q=","Data@odata.type":"Edm.Binary"}`
	const second = `{"PartitionKey":"data","RowKey":"000000001","Data":"c2Vjb25k","Data@odata.type":"Edm.Binary"}`
	const third = `{"PartitionKey":"data","RowKey":"000000002","Data":"dGhpcmQ=","Data@odata.type":"Edm.Binary"}`
	pages := []struct{ from, entities, want string }{
		{"000000000", "", ""},
		{"000000000", first + "," + third, "first"},
		{"000000001", third, ""},
		{"000000001", second + "," + third, "secondthird"},
		{"000000003", "", ""},
	}
	calls := 0
	client, err := aztables.NewClientWithNoCredential("https://read.invalid/session", &aztables.ClientOptions{ClientOptions: azcore.ClientOptions{
		Retry: policy.RetryOptions{MaxRetries: -1}, Transport: rotationHTTP(func(r *http.Request) (*http.Response, error) {
			if calls >= len(pages) {
				return nil, errors.New("unexpected poll")
			}
			page := pages[calls]
			calls++
			if got := r.URL.Query().Get("$filter"); got != "PartitionKey eq 'data' and RowKey ge '"+page.from+"'" {
				t.Errorf("filter = %q", got)
			}
			return tableReadResponse(r, 200, `{"value":[`+page.entities+`]}`), nil
		}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	tr := &tableTransport{rxClient: client}
	var got []byte
	for _, page := range pages {
		body, err := tr.ReadRaw(context.Background())
		if page.want == "" {
			if body != nil || !errors.Is(err, ErrNoData) {
				t.Fatalf("empty/gapped poll = %v, %v", body, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(body)
		body.Close()
		if err != nil || string(data) != page.want {
			t.Fatalf("read = %q, %v; want %q", data, err, page.want)
		}
		got = append(got, data...)
	}
	if string(got) != "firstsecondthird" {
		t.Fatalf("out of order: %q", got)
	}
}

func TestBlobReadEmptyPollingPreservesProgress(t *testing.T) {
	for _, emptyStatus := range []int{http.StatusOK, http.StatusNotFound, http.StatusRequestedRangeNotSatisfiable} {
		t.Run(strconv.Itoa(emptyStatus), func(t *testing.T) {
			calls := 0
			empty := &interruptedBlobBody{err: io.EOF}
			tr := newReadTestBlob(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if calls > 1 && blobRequestRange(r) != "bytes=5-" {
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
