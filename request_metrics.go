package aznet

import (
	"net/http"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

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
