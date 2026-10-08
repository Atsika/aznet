package aznet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/data/aztables"
	"github.com/google/uuid"
)

// The real SDK must request a bodyless acknowledgement while preserving the
// exact entity on an uncertain retry. Simulate commit followed by response loss,
// then Azure's EntityAlreadyExists receipt, as well as ordinary 204 success.
func TestTableWriteOmitsResponseEcho(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprint(uncertain), func(t *testing.T) {
			calls := 0
			var first []byte
			lost := errors.New("response lost after commit")
			client, err := aztables.NewClientWithNoCredential("https://table.invalid/session", &aztables.ClientOptions{ClientOptions: azcore.ClientOptions{
				Retry: policy.RetryOptions{MaxRetries: -1},
				Transport: rotationHTTP(func(r *http.Request) (*http.Response, error) {
					calls++
					if got := r.Header.Get("Prefer"); got != "return-no-content" {
						t.Errorf("Prefer = %q; unused entity echo still requested", got)
					}
					body, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					if calls == 1 {
						first = body
					} else if !bytes.Equal(body, first) {
						t.Error("retry changed entity")
					}
					if uncertain {
						if calls == 1 {
							return nil, lost
						}
						return tableReadResponse(r, 409, `{"odata.error":{"code":"EntityAlreadyExists","message":{"value":"exists"}}}`), nil
					}
					return tableReadResponse(r, 204, ""), nil
				}),
			}})
			if err != nil {
				t.Fatal(err)
			}
			tr := &tableTransport{txClient: client}
			payload := bytes.Repeat([]byte{42}, 64<<10)
			write := func() error { return tr.WriteRaw(context.Background(), 0, bytes.NewReader(payload)) }
			if err := write(); uncertain {
				if !errors.Is(err, lost) || tr.txHasConfirmed {
					t.Fatalf("uncertain write: %v, confirmed=%t", err, tr.txHasConfirmed)
				}
				if err := write(); err != nil {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(extractTableData(first), payload) {
				t.Fatal("stored payload differs")
			}
			confirmedCalls := calls
			if err := write(); err != nil {
				t.Fatal(err)
			}
			if calls != confirmedCalls || !tr.txHasConfirmed {
				t.Fatal("confirmed retry sent again")
			}
		})
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

func TestTableDecodeErrorsPreserveCause(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{"base64", `"!!!!"`},
		{"json_type", `123`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, rx := newTableStore(t)
			key := formatRowKey(0)
			s.rows[key] = json.RawMessage(fmt.Sprintf(`{"PartitionKey":"data","RowKey":%q,"Data":%s}`, key, tc.data))
			var first error
			for range 2 {
				body, err := rx.ReadRaw(context.Background())
				if body != nil || err == nil {
					t.Fatalf("ReadRaw = %v, %v; want decode error", body, err)
				}
				if errors.Is(err, ErrBufferLimit) {
					t.Errorf("malformed row classified as overflow: %v", err)
				}
				var corrupt base64.CorruptInputError
				var wrongType *json.UnmarshalTypeError
				if tc.name == "base64" && !errors.As(err, &corrupt) || tc.name == "json_type" && !errors.As(err, &wrongType) {
					t.Errorf("typed decode cause lost: %v", err)
				}
				if first != nil && err != first {
					t.Fatal("decode failure was not retained")
				}
				first = err
			}
			if s.requests[http.MethodGet] != 1 || rx.rxSeq != 0 || len(rx.pending) != 0 {
				t.Fatal("malformed row retried or consumed data")
			}
		})
	}
}

func TestTableByteBoundAndSequenceExhaustion(t *testing.T) {
	s, tx, rx := newTableStore(t)
	if err := tx.WriteRaw(context.Background(), 0, strings.NewReader("123456789")); err != nil {
		t.Fatal(err)
	}
	WithBufferLimits(BufferLimits{Pending: 8})(rx.cfg)
	for range 2 {
		if _, err := rx.ReadRaw(context.Background()); !errors.Is(err, ErrBufferLimit) {
			t.Fatal(err)
		}
	}
	if s.requests[http.MethodGet] != 1 || rx.rxSeq != 0 || len(rx.pending) != 0 {
		t.Fatal("overflow retried or consumed data")
	}
	if err := tx.WriteRaw(context.Background(), 1_000_000_000, strings.NewReader("wrap")); !errors.Is(err, ErrBufferLimit) {
		t.Fatal(err)
	}
	if s.requests[http.MethodPost] != 1 {
		t.Fatal("sequence wrapped to existing row")
	}
}

// tableStore exercises the SDK HTTP boundary, including committed operations
// whose responses are lost. Tests control faults; no live Azure is implied.
type tableStore struct {
	mu              sync.Mutex
	rows            map[string]json.RawMessage
	requests        map[string]int
	uncertainWrite  string
	uncertainDelete string
	deleteFailure   string
	blockDelete     chan struct{}
	peakRows        int
	cleanupPasses   int
	lastMethod      string
	batchFailure    bool
	uncertainBatch  bool
}

var rowKeyPattern = regexp.MustCompile(`RowKey='([0-9]+)'`)

var filterKeyPattern = regexp.MustCompile(`RowKey ge '([0-9]+)'`)

func (s *tableStore) Do(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests[r.Method]++
	isBatch := strings.HasSuffix(r.URL.Path, "/$batch")
	if isBatch {
		s.requests["BATCH"]++
		s.cleanupPasses++
	} else if r.Method == http.MethodDelete && s.lastMethod != http.MethodDelete {
		s.cleanupPasses++
	}
	s.lastMethod = r.Method
	if (isBatch || r.Method == http.MethodDelete) && s.blockDelete != nil {
		close(s.blockDelete)
		s.blockDelete = nil
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
	if isBatch {
		return s.batchLocked(r)
	}
	respond := func(code int, body string) (*http.Response, error) { return tableReadResponse(r, code, body), nil }
	switch r.Method {
	case http.MethodPost:
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		var meta struct{ RowKey string }
		if err := json.Unmarshal(data, &meta); err != nil {
			return nil, err
		}
		if previous, ok := s.rows[meta.RowKey]; ok {
			if !bytes.Equal(previous, data) {
				return nil, errors.New("retry changed entity bytes")
			}
			return respond(409, `{"odata.error":{"code":"EntityAlreadyExists","message":{"value":"exists"}}}`)
		}
		s.rows[meta.RowKey] = data
		s.peakRows = max(s.peakRows, len(s.rows))
		if s.uncertainWrite == meta.RowKey {
			s.uncertainWrite = ""
			return nil, io.ErrUnexpectedEOF
		}
		return respond(204, "")
	case http.MethodDelete:
		key := rowKeyPattern.FindStringSubmatch(r.URL.Path)[1]
		if s.deleteFailure == key {
			s.deleteFailure = ""
			return respond(403, `{"odata.error":{"code":"AuthorizationFailure","message":{"value":"injected"}}}`)
		}
		_, exists := s.rows[key]
		delete(s.rows, key)
		if s.uncertainDelete == key {
			s.uncertainDelete = ""
			return nil, io.ErrUnexpectedEOF
		}
		if !exists {
			return respond(404, `{"odata.error":{"code":"ResourceNotFound","message":{"value":"absent"}}}`)
		}
		return respond(204, "")
	case http.MethodGet:
		from := filterKeyPattern.FindStringSubmatch(r.URL.Query().Get("$filter"))[1]
		limit, _ := strconv.Atoi(r.URL.Query().Get("$top"))
		var keys []string
		for key := range s.rows {
			if key >= from {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		keys = keys[:min(len(keys), limit)]
		var entities []string
		for _, key := range keys {
			entities = append(entities, string(s.rows[key]))
		}
		return respond(200, `{"value":[`+strings.Join(entities, ",")+`]}`)
	}
	return nil, fmt.Errorf("unexpected request %s %s", r.Method, r.URL)
}

func (s *tableStore) batchLocked(r *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	decoded, err := url.PathUnescape(string(body))
	if err != nil {
		return nil, err
	}
	keys := rowKeyPattern.FindAllStringSubmatch(decoded, -1)
	if len(keys) == 0 || len(keys) > 100 || strings.Count(decoded, "DELETE ") != len(keys) || strings.Count(decoded, "PartitionKey='data'") != len(keys) {
		return nil, errors.New("invalid batch delete actions")
	}
	if s.batchFailure {
		s.batchFailure = false
		return tableBatchResponse(r, 403, 1), nil
	}
	// A missing entity rolls back the entire change set, not just that row.
	for _, key := range keys {
		if _, exists := s.rows[key[1]]; !exists {
			return tableBatchResponse(r, 404, 1), nil
		}
	}
	for _, key := range keys {
		delete(s.rows, key[1])
	}
	if s.uncertainBatch {
		s.uncertainBatch = false
		return nil, io.ErrUnexpectedEOF
	}
	return tableBatchResponse(r, 204, len(keys)), nil
}

func tableBatchResponse(r *http.Request, status, count int) *http.Response {
	var body strings.Builder
	body.WriteString("--batchresponse\r\nContent-Type: multipart/mixed; boundary=changesetresponse\r\n\r\n")
	for range count {
		fmt.Fprintf(&body, "--changesetresponse\r\nContent-Type: application/http\r\nContent-Transfer-Encoding: binary\r\n\r\nHTTP/1.1 %d %s\r\nContent-Length: 0\r\n\r\n\r\n", status, http.StatusText(status))
	}
	body.WriteString("--changesetresponse--\r\n--batchresponse--\r\n")
	resp := tableReadResponse(r, http.StatusAccepted, body.String())
	resp.Header.Set("Content-Type", "multipart/mixed; boundary=batchresponse")
	return resp
}

func newTableStore(t testing.TB) (*tableStore, *tableTransport, *tableTransport) {
	t.Helper()
	s := &tableStore{rows: make(map[string]json.RawMessage), requests: make(map[string]int)}
	client, err := aztables.NewClientWithNoCredential("https://table.invalid/session", &aztables.ClientOptions{ClientOptions: azcore.ClientOptions{
		Retry: policy.RetryOptions{MaxRetries: -1}, Transport: s,
	}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := applyConfig([]Option{WithPing(0)})
	t.Cleanup(cfg.cancel)
	return s, &tableTransport{txClient: client, cfg: cfg}, &tableTransport{rxClient: client, cfg: cfg}
}

func readTableBody(t *testing.T, tr *tableTransport) []byte {
	t.Helper()
	body, err := tr.ReadRaw(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestTableConsumptionAndUncertainRetry(t *testing.T) {
	s, tx, rx := newTableStore(t)
	ctx := context.Background()
	s.uncertainWrite = formatRowKey(0)
	if err := tx.WriteRaw(ctx, 0, strings.NewReader("first")); err == nil {
		t.Fatal("expected lost response")
	}
	// A retry before consumption still collides with the original row.
	if err := tx.WriteRaw(ctx, 0, strings.NewReader("first")); err != nil {
		t.Fatal(err)
	}
	s.uncertainWrite = formatRowKey(1)
	if err := tx.WriteRaw(ctx, 1, strings.NewReader("second")); err == nil {
		t.Fatal("expected lost response")
	}
	body, err := rx.ReadRaw(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rx.rxSeq != 0 || len(s.rows) != 2 {
		t.Fatal("fetch consumed or deleted rows")
	}
	part := make([]byte, 2)
	if _, err := io.ReadFull(body, part); err != nil {
		t.Fatal(err)
	}
	body.Close()
	if rx.rxSeq != 0 || len(s.rows) != 2 {
		t.Fatal("partial consumption deleted a row")
	}
	got := append(part, readTableBody(t, rx)...)
	if string(got) != "firstsecond" {
		t.Fatalf("read %q", got)
	}
	if _, err := rx.ReadRaw(ctx); !errors.Is(err, ErrNoData) {
		t.Fatal(err)
	}
	if len(s.rows) != 2 || s.rows[formatRowKey(1)] == nil {
		t.Fatal("latest uncertain write receipt deleted")
	}
	// This retry occurs after full consumption, below the cleanup threshold.
	if err := tx.WriteRaw(ctx, 1, strings.NewReader("second")); err != nil {
		t.Fatal(err)
	}
	if err := tx.WriteRaw(ctx, 2, strings.NewReader("third")); err != nil {
		t.Fatal(err)
	}
	if got := string(readTableBody(t, rx)); got != "third" {
		t.Fatal(got)
	}
	if _, err := rx.ReadRaw(ctx); !errors.Is(err, ErrNoData) {
		t.Fatal(err)
	}
	before := s.requests[http.MethodPost]
	if err := tx.WriteRaw(ctx, 0, strings.NewReader("first")); err != nil {
		t.Fatal(err)
	}
	if s.requests[http.MethodPost] != before || len(s.rows) != 3 {
		t.Fatal("confirmed retry recreated reclaimed data")
	}
}

func TestTablePartialDeletionFailures(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(strconv.FormatBool(uncertain), func(t *testing.T) {
			s, tx, rx := newTableStore(t)
			for i := range tableCleanupRows + 1 {
				if err := tx.WriteRaw(context.Background(), uint64(i), strings.NewReader(strconv.Itoa(i))); err != nil {
					t.Fatal(err)
				}
			}
			for rx.rxSeq < tableCleanupRows+1 {
				readTableBody(t, rx)
			}
			s.batchFailure = true
			if _, err := rx.ReadRaw(context.Background()); err == nil || errors.Is(err, ErrNoData) {
				t.Fatalf("batch failure hidden: %v", err)
			}
			if rx.reclaimSeq != 0 || len(s.rows) != tableCleanupRows+1 {
				t.Fatal("failed atomic batch advanced or partially committed")
			}
			if uncertain {
				s.uncertainDelete = formatRowKey(1)
			} else {
				s.deleteFailure = formatRowKey(1)
			}
			if _, err := rx.ReadRaw(context.Background()); err == nil || errors.Is(err, ErrNoData) {
				t.Fatalf("delete failure hidden: %v", err)
			}
			if rx.reclaimSeq != 1 || rx.rxSeq != tableCleanupRows+1 {
				t.Fatal("failed deletion changed progress")
			}
			if _, err := rx.ReadRaw(context.Background()); !errors.Is(err, ErrNoData) {
				t.Fatal(err)
			}
			if rx.reclaimSeq != tableCleanupRows || len(s.rows) != 1 || s.requests[http.MethodDelete] != tableCleanupRows+1 {
				t.Fatal("deletion retry lost cursor or retained old rows")
			}
		})
	}
}

func TestTableBatchReconciliation(t *testing.T) {
	for _, lostResponse := range []bool{false, true} {
		t.Run(strconv.FormatBool(lostResponse), func(t *testing.T) {
			s, tx, rx := newTableStore(t)
			ctx := context.Background()
			for seq := range tableCleanupRows + 1 {
				if err := tx.WriteRaw(ctx, uint64(seq), bytes.NewReader([]byte{byte(seq)})); err != nil {
					t.Fatal(err)
				}
			}
			var got []byte
			for rx.rxSeq < tableCleanupRows+1 {
				got = append(got, readTableBody(t, rx)...)
			}
			for seq, value := range got {
				if int(value) != seq {
					t.Fatal("byte ordering")
				}
			}
			if lostResponse {
				s.uncertainBatch = true
			} else {
				delete(s.rows, formatRowKey(7))
			}
			if _, err := rx.ReadRaw(ctx); err == nil || errors.Is(err, ErrNoData) {
				t.Fatalf("batch error hidden: %v", err)
			}
			wantRows := tableCleanupRows
			if lostResponse {
				wantRows = 1
			}
			if rx.reclaimSeq != 0 || len(s.rows) != wantRows {
				t.Fatal("unknown batch outcome advanced cursor or atomic rollback failed")
			}
			if _, err := rx.ReadRaw(ctx); !errors.Is(err, ErrNoData) {
				t.Fatal(err)
			}
			if rx.reclaimSeq != tableCleanupRows || len(s.rows) != 1 || s.rows[formatRowKey(tableCleanupRows)] == nil || s.requests["BATCH"] != 1 || s.requests[http.MethodDelete] != tableCleanupRows {
				t.Fatal("batch reconciliation lost range or latest receipt")
			}
			// A later cleanup range returns to batching after reconciliation completes.
			for seq := tableCleanupRows + 1; seq < 2*tableCleanupRows+1; seq++ {
				if err := tx.WriteRaw(ctx, uint64(seq), bytes.NewReader([]byte{byte(seq)})); err != nil {
					t.Fatal(err)
				}
			}
			got = nil
			for rx.rxSeq < 2*tableCleanupRows+1 {
				got = append(got, readTableBody(t, rx)...)
			}
			for i, value := range got {
				if int(value) != tableCleanupRows+1+i {
					t.Fatal("ordering after reconciliation")
				}
			}
			if _, err := rx.ReadRaw(ctx); !errors.Is(err, ErrNoData) {
				t.Fatal(err)
			}
			if s.requests["BATCH"] != 2 || s.requests[http.MethodDelete] != tableCleanupRows || len(s.rows) != 1 {
				t.Fatal("subsequent cleanup did not return to batching")
			}
		})
	}
}

func TestTableCleanupThresholdPreservesUncertainReceipt(t *testing.T) {
	s, tx, rx := newTableStore(t)
	ctx := context.Background()
	for seq := range tableCleanupRows {
		if err := tx.WriteRaw(ctx, uint64(seq), bytes.NewReader([]byte{byte(seq)})); err != nil {
			t.Fatal(err)
		}
	}
	var got []byte
	for len(got) < tableCleanupRows {
		got = append(got, readTableBody(t, rx)...)
	}
	for seq, value := range got {
		if int(value) != seq {
			t.Fatal("byte ordering")
		}
	}
	// At the threshold, one consumed row is still the protected retry receipt.
	for range 3 {
		if _, err := rx.ReadRaw(ctx); !errors.Is(err, ErrNoData) {
			t.Fatal(err)
		}
	}
	if s.requests[http.MethodDelete] != 0 || s.requests["BATCH"] != 0 {
		t.Fatal("idle polls cleaned below threshold")
	}
	seq := tableCleanupRows
	s.uncertainWrite = formatRowKey(seq)
	payload := []byte{byte(seq), byte(seq)}
	if err := tx.WriteRaw(ctx, uint64(seq), bytes.NewReader(payload)); err == nil {
		t.Fatal("expected lost write response")
	}
	body, err := rx.ReadRaw(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, 1)
	if _, err := io.ReadFull(body, first); err != nil {
		t.Fatal(err)
	}
	body.Close()
	if s.requests[http.MethodDelete] != 0 || rx.rxSeq != seq {
		t.Fatal("partial consumption triggered cleanup")
	}
	if tail := readTableBody(t, rx); !bytes.Equal(append(first, tail...), payload) {
		t.Fatal("partial-read identity")
	}
	if _, err := rx.ReadRaw(ctx); !errors.Is(err, ErrNoData) {
		t.Fatal(err)
	}
	if s.cleanupPasses != 1 || s.requests["BATCH"] != 1 || s.requests[http.MethodDelete] != 0 || len(s.rows) != 1 || s.rows[formatRowKey(seq)] == nil {
		t.Fatal("threshold cleanup lost receipt or wrong range")
	}
	// The latest uncertain write still collides byte-for-byte after cleanup.
	if err := tx.WriteRaw(ctx, uint64(seq), bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	if err := tx.WriteRaw(ctx, 0, bytes.NewReader([]byte{0})); err != nil {
		t.Fatal(err)
	}
	if len(s.rows) != 1 {
		t.Fatal("confirmed old retry recreated reclaimed row")
	}
}

func TestTableReclamationShutdown(t *testing.T) {
	s, tx, rx := newTableStore(t)
	a, b := reviewNoise(t)
	for i := range tableCleanupRows + 1 {
		var frame bytes.Buffer
		BuildFrame(&frame, Frame{Type: MsgTypeData, Payload: []byte{byte(i)}})
		raw, _ := a.SealData(nil, frame.Bytes())
		if err := tx.WriteRaw(context.Background(), uint64(i), bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
	}
	c := reviewConn(rx, b)
	// Give Close a real SDK writer. Its request waits behind the blocked delete
	// until the bounded Close cancels connection I/O.
	rx.txClient = tx.txClient
	if _, err := io.ReadFull(c, make([]byte, tableCleanupRows+1)); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	s.blockDelete = entered
	done := make(chan error, 1)
	go func() { _, err := c.Read(make([]byte, 1)); done <- err }()
	<-entered
	start := time.Now()
	if err := c.Close(); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Close = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("Close waited for deletion indefinitely")
	}
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("reclamation ignored shutdown")
	}
	if rx.reclaimSeq != 0 {
		t.Fatal("canceled deletion advanced")
	}
}

func TestTableConnUncertainWriteAfterConsumption(t *testing.T) {
	s, tx, rx := newTableStore(t)
	a, b := reviewNoise(t)
	sender, receiver := reviewConn(tx, a), reviewConn(rx, b)
	defer sender.cancel()
	defer receiver.cancel()
	s.uncertainWrite = formatRowKey(0)
	if n, err := sender.Write([]byte("first")); n != 5 || err != nil {
		t.Fatal(n, err)
	}
	owedFailure(t, sender)
	got := make([]byte, 5)
	if _, err := io.ReadFull(receiver, got); err != nil || string(got) != "first" {
		t.Fatal(string(got), err)
	}
	// The HTTP fixture compares the retry entity byte-for-byte with the original.
	if err := sender.flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Write([]byte("second")); err != nil {
		t.Fatal(err)
	}
	got = make([]byte, 6)
	if _, err := io.ReadFull(receiver, got); err != nil || string(got) != "second" {
		t.Fatal(string(got), err)
	}
	if _, err := rx.ReadRaw(context.Background()); !errors.Is(err, ErrNoData) {
		t.Fatal(err)
	}
	if len(s.rows) != 2 || s.requests[http.MethodDelete] != 0 {
		t.Fatal("cleanup ran below threshold")
	}
}

func TestTableReclamationRequestMeasurement(t *testing.T) {
	for _, rows := range []int{1, 4} {
		for _, size := range []int{64, 32 << 10, MaxTableEntitySize} {
			t.Run(fmt.Sprintf("rows%d_bytes%d", rows, size), func(t *testing.T) {
				s, tx, rx := newTableStore(t)
				WithBufferLimits(BufferLimits{Pending: rows * MaxTableEntitySize})(rx.cfg)
				payload := bytes.Repeat([]byte{42}, size)
				const count = 16
				for batch := 0; batch < count; batch += rows {
					for i := batch; i < min(batch+rows, count); i++ {
						if err := tx.WriteRaw(context.Background(), uint64(i), bytes.NewReader(payload)); err != nil {
							t.Fatal(err)
						}
					}
					if got := readTableBody(t, rx); !bytes.Equal(got, bytes.Repeat(payload, min(rows, count-batch))) {
						t.Fatal("bulk byte identity")
					}
				}
				if _, err := rx.ReadRaw(context.Background()); !errors.Is(err, ErrNoData) {
					t.Fatal(err)
				}
				if len(s.rows) != count || s.requests[http.MethodDelete] != 0 {
					t.Fatal("cleanup ran below threshold")
				}
				t.Logf("chunks=%d payload=%d prefetch=%d peak_rows=%d retained=%d GET=%d POST=%d DELETE=%d", count, size, rows, s.peakRows, len(s.rows), s.requests[http.MethodGet], s.requests[http.MethodPost], s.requests[http.MethodDelete])
			})
		}
	}
}

func TestTableSlowConsumerRetainsUnreadRows(t *testing.T) {
	for _, count := range []int{64, 304} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			for _, concurrency := range []int{1, 8} {
				t.Run(strconv.Itoa(concurrency), func(t *testing.T) {
					for i := range concurrency {
						t.Run(strconv.Itoa(i), func(t *testing.T) {
							t.Parallel()
							s, tx, rx := newTableStore(t)
							for seq := range count {
								if err := tx.WriteRaw(context.Background(), uint64(seq), bytes.NewReader([]byte{byte(seq)})); err != nil {
									t.Fatal(err)
								}
							}
							if len(s.rows) != count || s.requests[http.MethodDelete] != 0 {
								t.Fatal("unread data reclaimed")
							}
							var got []byte
							for len(got) < count {
								got = append(got, readTableBody(t, rx)...)
							}
							for seq, value := range got {
								if value != byte(seq) {
									t.Fatal("slow consumer byte order")
								}
							}
							if _, err := rx.ReadRaw(context.Background()); !errors.Is(err, ErrNoData) {
								t.Fatal(err)
							}
							wantBatches := (count - 1) / tableCleanupRows
							if len(s.rows) != count-wantBatches*tableCleanupRows || s.cleanupPasses != wantBatches || s.requests["BATCH"] != wantBatches || s.requests[http.MethodDelete] != 0 {
								t.Fatal("cleanup threshold did not bound retained rows/passes")
							}
							t.Logf("connections=%d peak_unread_rows=%d retained=%d GET=%d POST=%d DELETE=%d BATCH=%d cleanup_passes=%d", concurrency, s.peakRows, len(s.rows), s.requests[http.MethodGet], s.requests[http.MethodPost], s.requests[http.MethodDelete], s.requests["BATCH"], s.cleanupPasses)
						})
					}
				})
			}
		})
	}
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
			if r.Method == http.MethodDelete {
				return tableReadResponse(r, 204, ""), nil
			}
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
			var response *http.Response
			writeCtx := policy.WithCaptureResponse(ctx, &response)
			if err := tx.WriteRaw(writeCtx, uint64(seq), bytes.NewReader(payload)); err != nil {
				t.Fatal(err)
			}
			if response == nil || response.StatusCode != http.StatusNoContent || response.ContentLength != 0 {
				t.Fatal("session insert did not receive a bodyless acknowledgement")
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
			// Azurite 3.34.0 authenticates the outer $batch as a table named
			// "$batch", rejecting table-scoped SAS. Exercise reconciliation
			// only for that observed error; shared-key batching must succeed.
			var responseErr *azcore.ResponseError
			if rx != client || !errors.As(err, &responseErr) || responseErr.StatusCode != 403 || responseErr.ErrorCode != "AuthorizationFailure" {
				t.Fatal(err)
			}
			if rx.reclaimSeq != 0 || !rx.reclaimSingles {
				t.Fatal("failed batch advanced cleanup")
			}
			t.Log("Azurite rejected table-SAS batch: validating individual reconciliation; live Azure SAS batch remains unvalidated")
			if _, err := rx.ReadRaw(ctx); !errors.Is(err, ErrNoData) {
				t.Fatal(err)
			}
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

// BenchmarkTableChunk moves full-size sealed chunks through the real SDK and
// an in-memory Table fixture, isolating driver encoding cost from the network.
func BenchmarkTableChunk(b *testing.B) {
	_, tx, rx := newTableStore(b)
	ctx := context.Background()
	chunk := bytes.Repeat([]byte{42}, MaxTableEntitySize)
	b.SetBytes(int64(len(chunk)))
	b.ReportAllocs()
	for i := range b.N {
		if err := tx.WriteRaw(ctx, uint64(i), bytes.NewReader(chunk)); err != nil {
			b.Fatal(err)
		}
		body, err := rx.ReadRaw(ctx)
		if err != nil {
			b.Fatal(err)
		}
		if n, err := io.Copy(io.Discard, body); err != nil || n != int64(len(chunk)) {
			b.Fatal(n, err)
		}
		body.Close()
	}
}
