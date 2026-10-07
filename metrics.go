package aznet

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

// Metrics is an interface for tracking connection statistics.
// Azure adapters count every SDK attempt, including failures and retries.
// Byte counters track successful transport payload writes and consumed reads,
// including framing/handshakes; they are neither HTTP wire bytes nor billed usage.
type Metrics interface {
	IncrementWriteTransaction()
	IncrementReadTransaction()
	IncrementListTransaction()
	IncrementDeleteTransaction()
	IncrementBytesSent(n int64)
	IncrementBytesReceived(n int64)

	GetWriteTransactionCount() int64
	GetReadTransactionCount() int64
	GetListTransactionCount() int64
	GetDeleteTransactionCount() int64
	GetBytesSent() int64
	GetBytesReceived() int64
}

// DefaultMetrics uses atomic aggregate counters and a synchronized attempt map.
type DefaultMetrics struct {
	requests           requestCounts
	writeTransactions  int64
	readTransactions   int64
	listTransactions   int64
	deleteTransactions int64
	bytesSent          int64
	bytesReceived      int64
}

// NewDefaultMetrics creates a new DefaultMetrics instance.
func NewDefaultMetrics() *DefaultMetrics { return &DefaultMetrics{} }

func (m *DefaultMetrics) IncrementWriteTransaction()     { atomic.AddInt64(&m.writeTransactions, 1) }
func (m *DefaultMetrics) IncrementReadTransaction()      { atomic.AddInt64(&m.readTransactions, 1) }
func (m *DefaultMetrics) IncrementListTransaction()      { atomic.AddInt64(&m.listTransactions, 1) }
func (m *DefaultMetrics) IncrementDeleteTransaction()    { atomic.AddInt64(&m.deleteTransactions, 1) }
func (m *DefaultMetrics) IncrementBytesSent(n int64)     { atomic.AddInt64(&m.bytesSent, n) }
func (m *DefaultMetrics) IncrementBytesReceived(n int64) { atomic.AddInt64(&m.bytesReceived, n) }

func (m *DefaultMetrics) GetWriteTransactionCount() int64 {
	return atomic.LoadInt64(&m.writeTransactions)
}
func (m *DefaultMetrics) GetReadTransactionCount() int64 {
	return atomic.LoadInt64(&m.readTransactions)
}
func (m *DefaultMetrics) GetListTransactionCount() int64 {
	return atomic.LoadInt64(&m.listTransactions)
}
func (m *DefaultMetrics) GetDeleteTransactionCount() int64 {
	return atomic.LoadInt64(&m.deleteTransactions)
}
func (m *DefaultMetrics) GetBytesSent() int64     { return atomic.LoadInt64(&m.bytesSent) }
func (m *DefaultMetrics) GetBytesReceived() int64 { return atomic.LoadInt64(&m.bytesReceived) }

// GetMetrics returns the metrics from a connection if it supports metrics tracking.
// It returns nil if the connection doesn't support metrics.
func GetMetrics(c net.Conn) Metrics {
	type metricsProvider interface{ GetMetrics() Metrics }
	if mp, ok := c.(metricsProvider); ok {
		return mp.GetMetrics()
	}
	return nil
}

type metricsDriver struct {
	Driver
	m Metrics
}

func (d *metricsDriver) PostHandshake(ctx context.Context, connID string, data []byte) error {
	err := d.Driver.PostHandshake(ctx, connID, data)
	if err == nil {
		d.m.IncrementBytesSent(int64(len(data)))
	}
	return err
}

func (d *metricsDriver) PostToken(ctx context.Context, connID string, data []byte) error {
	err := d.Driver.PostToken(ctx, connID, data)
	if err == nil {
		d.m.IncrementBytesSent(int64(len(data)))
	}
	return err
}

func (d *metricsDriver) GetToken(ctx context.Context, connID string) ([]byte, error) {
	data, err := d.Driver.GetToken(ctx, connID)
	if err == nil {
		d.m.IncrementBytesReceived(int64(len(data)))
	}
	return data, err
}

func (d *metricsDriver) NewTransport(ctx context.Context, connID string, tokens SessionTokens, isInitiator bool) (Transport, error) {
	t, err := d.Driver.NewTransport(ctx, connID, tokens, isInitiator)
	if err != nil {
		// Preserve partial acquisitions so the core can close them on rollback.
		return t, err
	}
	return newMetricsTransport(t, d.m), nil
}

// RequestAttempt identifies one SDK pipeline attempt. Labels never contain URLs,
// resource names, tokens or error text. StatusCode is zero without a response.
// Retry is true only for an SDK retry, not a new application-level invocation.
// Failed includes transport errors and HTTP status codes >= 400. A successful
// batch HTTP response may still contain failed operations inside its body.
type RequestAttempt struct {
	Driver     string
	Operation  string
	Method     string
	StatusCode int
	Retry      bool
	Failed     bool
}

// RequestMetrics is an optional extension to Metrics for detailed SDK attempts.
// RecordRequest must be safe for concurrent use and must not block I/O.
// Drivers that do not use Azure's SDK can implement their own instrumentation.
type RequestMetrics interface{ RecordRequest(RequestAttempt) }

type requestCounts struct {
	mu     sync.Mutex
	counts map[RequestAttempt]int64
}

// RecordRequest records one completed SDK pipeline attempt.
func (m *DefaultMetrics) RecordRequest(a RequestAttempt) {
	m.requests.mu.Lock()
	defer m.requests.mu.Unlock()
	if m.requests.counts == nil {
		m.requests.counts = make(map[RequestAttempt]int64)
	}
	m.requests.counts[a]++
}

// RequestCounts returns an independent snapshot of completed SDK attempts.
func (m *DefaultMetrics) RequestCounts() map[RequestAttempt]int64 {
	m.requests.mu.Lock()
	defer m.requests.mu.Unlock()
	counts := make(map[RequestAttempt]int64, len(m.requests.counts))
	for k, v := range m.requests.counts {
		counts[k] = v
	}
	return counts
}

type requestMetricsPolicy struct {
	driver         string
	metrics        Metrics
	resourceOffset int
}
type requestAttemptNumber struct{ count int }

type requestAttemptPolicy struct{}

func (requestAttemptPolicy) Do(req *policy.Request) (*http.Response, error) {
	req.SetOperationValue(&requestAttemptNumber{})
	return req.Next()
}

func sdkClientOptions(driver string, m Metrics, ep *Endpoint) azcore.ClientOptions {
	offset := 0
	if !ep.IsAzure {
		offset = 1
	}
	return azcore.ClientOptions{PerCallPolicies: []policy.Policy{requestAttemptPolicy{}}, PerRetryPolicies: []policy.Policy{&requestMetricsPolicy{driver: driver, metrics: m, resourceOffset: offset}}}

}

func (p *requestMetricsPolicy) Do(req *policy.Request) (*http.Response, error) {
	var number *requestAttemptNumber
	req.OperationValue(&number)
	if number == nil {
		number = &requestAttemptNumber{}
		req.SetOperationValue(number)
	}
	number.count++
	attempt := RequestAttempt{Driver: p.driver, Operation: requestOperation(p.driver, req.Raw(), p.resourceOffset), Method: req.Raw().Method, Retry: number.count > 1}
	resp, err := req.Next()
	if resp != nil {
		attempt.StatusCode = resp.StatusCode
	}
	attempt.Failed = err != nil || attempt.StatusCode >= 400
	if p.metrics != nil {
		switch {
		case attempt.Operation == "ListBlobs" || attempt.Operation == "QueryEntities":
			p.metrics.IncrementListTransaction()
		case attempt.Method == http.MethodGet || attempt.Method == http.MethodHead:
			p.metrics.IncrementReadTransaction()
		case attempt.Method == http.MethodDelete:
			p.metrics.IncrementDeleteTransaction()
		default:
			p.metrics.IncrementWriteTransaction()
		}
		if detailed, ok := p.metrics.(RequestMetrics); ok {
			detailed.RecordRequest(attempt)
		}
	}
	return resp, err
}

func requestOperation(driver string, r *http.Request, resourceOffset int) string {
	q := r.URL.Query()
	switch driver {
	case blobDriverName:
		switch q.Get("comp") {
		case "list":
			return "ListBlobs"
		case "appendblock":
			return "AppendBlock"
		}
		if q.Get("restype") == "container" {
			if r.Method == http.MethodDelete {
				return "DeleteContainer"
			}
			return "CreateContainer"
		}
		switch r.Method {
		case http.MethodGet:
			return "DownloadBlob"
		case http.MethodDelete:
			return "DeleteBlob"
		case http.MethodPut:
			return "PutBlob"
		case http.MethodHead:
			return "GetBlobProperties"
		}
	case queueDriverName:
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) > resourceOffset+1 && parts[resourceOffset+1] == "messages" {
			switch r.Method {
			case http.MethodGet:
				if q.Get("peekonly") == "true" {
					return "PeekMessages"
				}
				return "DequeueMessages"
			case http.MethodPost:
				return "EnqueueMessage"
			case http.MethodDelete:
				return "DeleteMessage"
			case http.MethodPut:
				return "UpdateMessage"
			}
		}
		if r.Method == http.MethodDelete {
			return "DeleteQueue"
		}
		if r.Method == http.MethodPut {
			return "CreateQueue"
		}
	case tableDriverName:
		if strings.HasSuffix(r.URL.Path, "/$batch") {
			return "SubmitTransaction"
		}
		if strings.HasSuffix(r.URL.Path, "/Tables") {
			return "CreateTable"
		}
		if strings.Contains(r.URL.Path, "/Tables(") {
			return "DeleteTable"
		}
		switch r.Method {
		case http.MethodGet:
			if strings.Contains(r.URL.Path, "PartitionKey=") {
				return "GetEntity"
			}
			return "QueryEntities"
		case http.MethodDelete:
			return "DeleteEntity"
		case http.MethodPost:
			return "AddEntity"
		case http.MethodPatch:
			return "UpdateEntity"
		case http.MethodPut:
			return "UpdateEntity"
		}
	}
	return "Other"
}

type metricsTransport struct {
	Transport
	rot Rotator // nil if underlying transport doesn't support rotation
	m   Metrics
}

type metricsLimitedTransport struct {
	*metricsTransport
	reader limitedRawReader
}

func newMetricsTransport(t Transport, m Metrics) Transport {
	mt := &metricsTransport{Transport: t, m: m}
	if r, ok := t.(Rotator); ok {
		mt.rot = r
	}
	if reader, ok := t.(limitedRawReader); ok {
		return &metricsLimitedTransport{metricsTransport: mt, reader: reader}
	}
	return mt
}

func (t *metricsTransport) WriteRaw(ctx context.Context, seq uint64, data io.ReadSeeker) error {
	var size int64
	if data != nil {
		pos, _ := data.Seek(0, io.SeekCurrent)
		end, _ := data.Seek(0, io.SeekEnd)
		_, _ = data.Seek(pos, io.SeekStart)
		size = end - pos
	}
	err := t.Transport.WriteRaw(ctx, seq, data)
	if err == nil {
		t.m.IncrementBytesSent(size)
	}
	return err
}

func (t *metricsTransport) ReadRaw(ctx context.Context) (io.ReadCloser, error) {
	rc, err := t.Transport.ReadRaw(ctx)
	return t.recordRead(rc, err)
}

func (t *metricsLimitedTransport) ReadRawLimit(ctx context.Context, limit int) (io.ReadCloser, error) {
	rc, err := t.reader.ReadRawLimit(ctx, limit)
	return t.recordRead(rc, err)
}

func (t *metricsTransport) recordRead(rc io.ReadCloser, err error) (io.ReadCloser, error) {
	if err == nil {
		return &metricsReadCloser{ReadCloser: rc, m: t.m}, nil
	}
	return nil, err
}

func (t *metricsTransport) ShouldRotate() bool {
	if t.rot != nil {
		return t.rot.ShouldRotate()
	}
	return false
}

func (t *metricsTransport) RotateTX(ctx context.Context) error {
	if t.rot != nil {
		return t.rot.RotateTX(ctx)
	}
	return nil
}

func (t *metricsTransport) RotateRX() error {
	if t.rot != nil {
		return t.rot.RotateRX()
	}
	return nil
}

type metricsReadCloser struct {
	io.ReadCloser
	m Metrics
}

func (r *metricsReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.m.IncrementBytesReceived(int64(n))
	}
	return n, err
}

func (d *metricsDriver) CreateBootstrapTokensFor(duration time.Duration) (string, string, error) {
	issuer, ok := d.Driver.(BootstrapTokenIssuer)
	if !ok {
		return "", "", ErrBootstrapDurationUnsupported
	}
	return issuer.CreateBootstrapTokensFor(duration)
}
