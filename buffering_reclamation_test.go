package aznet

import (
	"bytes"
	"context"
	"encoding/binary"
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
)

func TestBoundedWriteBatchesAndCiphertextRetry(t *testing.T) {
	a, b := reviewNoise(t)
	var attempts [][]byte
	var seqs []uint64
	var c *Conn
	var plain []byte
	cfg := applyConfig([]Option{WithPing(0), WithBufferLimits(BufferLimits{Write: 64, Retry: 40})})
	c = newConn(cfg.ctx, cfg.cancel, &reviewTransport{write: func(_ context.Context, seq uint64, r io.ReadSeeker) error {
		raw, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		if len(raw) > 40 || c.bufs.Write.Len() > 64 {
			t.Fatal("byte allowance exceeded")
		}
		attempts = append(attempts, raw)
		seqs = append(seqs, seq)
		if len(attempts) == 1 {
			return io.ErrUnexpectedEOF
		}
		p, _, err := b.UnsealData(nil, raw, 40)
		plain = append(plain, p...)
		return err
	}}, cfg, a, nil, "bounded")
	defer c.cancel()
	want := bytes.Repeat([]byte("0123456789"), 100)
	n, err := c.Write(want)
	if !errors.Is(err, io.ErrUnexpectedEOF) || n <= 0 || n >= len(want) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if len(c.pending.data) > 40 || c.bufs.Write.Len() > 64 {
		t.Fatal("retry exceeded allowance")
	}
	if err := c.flush(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(attempts[0], attempts[1]) || seqs[0] != seqs[1] {
		t.Fatal("retry changed bytes or sequence")
	}
	if got, err := c.Write(want[n:]); err != nil || got != len(want)-n {
		t.Fatal(got, err)
	}
	var got []byte
	for len(plain) > 0 {
		length := int(binary.BigEndian.Uint32(plain[:4]))
		got = append(got, plain[FrameHeaderSize:FrameHeaderSize+length]...)
		plain = plain[FrameHeaderSize+length:]
	}
	if !bytes.Equal(got, want) {
		t.Fatal("batched bytes reordered, duplicated or lost")
	}
	if c.pending.data != nil {
		t.Fatal("successful retry still retains ciphertext")
	}
}

func TestReceiveByteOverflowIsTerminal(t *testing.T) {
	for _, stage := range []string{"pending", "decrypted", "frame"} {
		t.Run(stage, func(t *testing.T) {
			a, b := reviewNoise(t)
			var raw []byte
			limits := BufferLimits{Pending: 128, Decrypted: 32}
			switch stage {
			case "pending":
				raw = bytes.Repeat([]byte{1}, 129)
			case "decrypted":
				raw, _ = a.SealData(nil, bytes.Repeat([]byte{1}, 33))
			case "frame":
				var header [FrameHeaderSize]byte
				binary.BigEndian.PutUint32(header[:4], 100)
				raw, _ = a.SealData(nil, header[:])
			}
			calls := 0
			c := reviewConn(&reviewTransport{read: func(context.Context) (io.ReadCloser, error) {
				calls++
				return io.NopCloser(bytes.NewReader(raw)), nil
			}}, b)
			WithBufferLimits(limits)(c.cfg)
			defer c.cancel()
			for range 2 {
				if _, err := c.Read(make([]byte, 1)); !errors.Is(err, ErrBufferLimit) {
					t.Fatalf("Read = %v", err)
				}
			}
			if _, err := c.Write([]byte("x")); !errors.Is(err, ErrBufferLimit) {
				t.Fatalf("Write after overflow = %v", err)
			}
			if calls != 1 || c.bufs.Noise.Len() > 128 || c.bufs.Read.Len() > 32 {
				t.Fatal("overflow polled again or retained excess bytes")
			}
			start := time.Now()
			_ = c.Close()
			if time.Since(start) > time.Second {
				t.Fatal("overflow shutdown blocked")
			}
		})
	}
}

type zeroBeforeLastByte struct {
	*bytes.Reader
	paused bool
}

func (r *zeroBeforeLastByte) Read(p []byte) (int, error) {
	if r.Len() == 1 && !r.paused {
		r.paused = true
		return 0, nil
	}
	return r.Reader.Read(p)
}

func TestReceiveOverflowProbeDoesNotDiscardUnreadByte(t *testing.T) {
	a, b := reviewNoise(t)
	var frame bytes.Buffer
	BuildFrame(&frame, Frame{Type: MsgTypeData, Payload: make([]byte, 103)})
	raw, err := a.SealData(nil, frame.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, 42) // Exactly 128 valid bytes followed by one excess byte.
	c := reviewConn(&reviewTransport{read: func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(&zeroBeforeLastByte{Reader: bytes.NewReader(raw)}), nil
	}}, b)
	defer c.cancel()
	WithBufferLimits(BufferLimits{Pending: 128})(c.cfg)
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, ErrBufferLimit) {
		t.Fatal(err)
	}
}

func TestQueuePendingByteAccounting(t *testing.T) {
	cfg := applyConfig([]Option{WithBufferLimits(BufferLimits{Pending: 8})})
	defer cfg.cancel()
	tr := &queueTransport{cfg: cfg, pending: make(map[uint64][]byte)}
	if tr.ingestLocked(1, []byte("4567")) || tr.ingestLocked(1, []byte("duplicate")) {
		t.Fatal("duplicate counted")
	}
	if !tr.ingestLocked(2, []byte("overflow")) || tr.pendingBytes != 4 || len(tr.pending) != 1 {
		t.Fatal("overflow retained bytes")
	}
	if tr.ingestLocked(0, []byte("0123")) {
		t.Fatal("exact allowance rejected")
	}
	if got := string(tr.drainLocked()); got != "01234567" || tr.pendingBytes != 0 {
		t.Fatal(got, tr.pendingBytes)
	}
	if tr.ingestLocked(0, []byte("old")) || tr.pendingBytes != 0 {
		t.Fatal("consumed duplicate counted")
	}
}

func TestRepeatedWriteFailuresStayBounded(t *testing.T) {
	a, _ := reviewNoise(t)
	var first []byte
	cfg := applyConfig([]Option{WithPing(0), WithBufferLimits(BufferLimits{Write: 64, Retry: 40})})
	c := newConn(cfg.ctx, cfg.cancel, &reviewTransport{write: func(_ context.Context, seq uint64, r io.ReadSeeker) error {
		data, _ := io.ReadAll(r)
		if first == nil {
			first = data
		}
		if seq != 0 || !bytes.Equal(first, data) {
			t.Fatal("failed retry changed ciphertext")
		}
		return io.ErrUnexpectedEOF
	}}, cfg, a, nil, "retry")
	defer c.cancel()
	for range 10 {
		if _, err := c.Write(make([]byte, 100)); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatal(err)
		}
		if c.bufs.Write.Len() > 64 || len(c.pending.data) > 40 {
			t.Fatal("failures accumulated unbounded bytes")
		}
	}
	if err := c.CloseWrite(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	if c.bufs.Write.Len() > 64 {
		t.Fatal("FIN exceeded allowance")
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

func newTableStore(t *testing.T) (*tableStore, *tableTransport, *tableTransport) {
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
	if n, err := sender.Write([]byte("first")); n != 5 || err == nil {
		t.Fatal(n, err)
	}
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

type sizedReviewTransport struct {
	reviewTransport
	size int
}

func (t *sizedReviewTransport) MaxRawSize() int { return t.size }

func TestWriteAllowanceMeasurement(t *testing.T) {
	for _, allowance := range []int{4 << 20, 8 << 20, 16 << 20} {
		t.Run(strconv.Itoa(allowance), func(t *testing.T) {
			a, b := reviewNoise(t)
			var c *Conn
			requests, peak, received := 0, 0, 0
			tr := &sizedReviewTransport{size: 4 << 20}
			tr.write = func(_ context.Context, seq uint64, r io.ReadSeeker) error {
				if seq != uint64(requests) {
					t.Fatal("sequence ordering")
				}
				requests++
				peak = max(peak, c.bufs.Write.Len())
				raw, _ := io.ReadAll(r)
				plain, _, err := b.UnsealData(nil, raw, tr.size)
				if err != nil {
					return err
				}
				for len(plain) > 0 {
					length := int(binary.BigEndian.Uint32(plain[:4]))
					for _, value := range plain[FrameHeaderSize : FrameHeaderSize+length] {
						if value != 42 {
							t.Fatal("byte identity")
						}
					}
					received += length
					plain = plain[FrameHeaderSize+length:]
				}
				return nil
			}
			cfg := applyConfig([]Option{WithPing(0), WithBufferLimits(BufferLimits{Write: allowance})})
			defer cfg.cancel()
			c = newConn(cfg.ctx, cfg.cancel, tr, cfg, a, nil, "measurement")
			payload := bytes.Repeat([]byte{42}, 16<<20)
			if n, err := c.Write(payload); n != len(payload) || err != nil {
				t.Fatal(n, err)
			}
			if received != len(payload) || peak > allowance {
				t.Fatal("byte bounds or accounting")
			}
			t.Logf("payload=%d write_allowance=%d peak_write=%d requests=%d retry_limit=%d", len(payload), allowance, peak, requests, cfg.limits().Retry)
		})
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
