package aznet

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/data/aztables"
)

const tableDriverName = "aztable"

// tableReadRows amortizes queries without exposing storage tuning in Config.
const tableReadRows = 4

// tableCleanupRows defers deletion until a modest backlog has accumulated.
// The newest consumed row is never included: it remains the retry receipt.
const tableCleanupRows = 16

// MaxTableBinaryPropertySize is the maximum size (64 KiB) for a single Edm.Binary property.
const MaxTableBinaryPropertySize = 64 * 1024

// MaxTableProperties is the number of binary properties we use to store a single large entity.
const MaxTableProperties = 15
const MaxTableEntitySize = MaxTableProperties * MaxTableBinaryPropertySize

var dataKeys = [MaxTableProperties]string{"Data", "Data01", "Data02", "Data03", "Data04", "Data05", "Data06", "Data07", "Data08", "Data09", "Data10", "Data11", "Data12", "Data13", "Data14"}

func init() {
	RegisterFactory(tableDriverName, &tableFactory{})
}

func buildTableEntity(pk, rk string, data []byte) ([]byte, error) {
	m := map[string]any{"PartitionKey": pk, "RowKey": rk}
	for i := 0; i < MaxTableProperties && len(data) > 0; i++ {
		take := min(len(data), MaxTableBinaryPropertySize)
		m[dataKeys[i]], m[dataKeys[i]+"@odata.type"] = data[:take], "Edm.Binary"
		data = data[take:]
	}
	return json.Marshal(m)
}

func extractTableData(raw []byte) []byte {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	var res []byte
	for i := range MaxTableProperties {
		v, ok := m[dataKeys[i]]
		if !ok {
			break
		}
		chunk, _ := base64.StdEncoding.DecodeString(v.(string))
		res = append(res, chunk...)
	}
	return res
}

// Session data is bounded before base64 decoding. Bootstrap parsing has a
// separate lifetime; session rows must not bypass the receive byte allowance.
func extractSessionTableData(raw []byte, limit int) ([]byte, error) {
	var properties map[string]json.RawMessage
	if err := json.Unmarshal(raw, &properties); err != nil {
		return nil, err
	}
	limit = min(limit, MaxTableEntitySize)
	var data []byte
	for _, key := range dataKeys {
		value, ok := properties[key]
		if !ok {
			break
		}
		var encoded string
		if err := json.Unmarshal(value, &encoded); err != nil {
			return nil, err
		}
		// DecodedLen includes up to two padding bytes, checked exactly below.
		if base64.StdEncoding.DecodedLen(len(encoded)) > min(MaxTableBinaryPropertySize, limit-len(data))+2 {
			return nil, ErrBufferLimit
		}
		chunk, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, err
		}
		if len(chunk) > MaxTableBinaryPropertySize || len(chunk) > limit-len(data) {
			return nil, ErrBufferLimit
		}
		data = append(data, chunk...)
	}
	if len(data) == 0 {
		return nil, errors.New("empty session row")
	}
	return data, nil
}

type tableFactory struct{}

func (d *tableFactory) NewDriver(ep *Endpoint, cfg *Config) (Driver, error) {
	client, err := newTableClient(ep)
	if err != nil {
		return nil, err
	}
	if client != nil {
		for _, name := range []string{cfg.handshakeEndpoint, cfg.tokenEndpoint} {
			if _, err := client.CreateTable(cfg.ctx, name, nil); err != nil {
				var respErr *azcore.ResponseError
				if errors.As(err, &respErr) {
					switch aztables.TableErrorCode(respErr.ErrorCode) {
					case aztables.TableAlreadyExists:
						continue
					case aztables.TableBeingDeleted:
						// Azure holds a deleted table's name for ~40s; surface that as
						// the shared sentinel so callers can decide whether to wait.
						return nil, fmt.Errorf("%w: table %q: %w", ErrResourceBeingDeleted, name, err)
					}
				}
				// Previously ignored, which reported a healthy listener whose agents
				// then died on the first missing table.
				return nil, err
			}
		}
	}
	var hSAS, tSAS string
	if client == nil {
		hSAS, tSAS, _ = ep.ParseSAS(cfg)
	}
	ht, err := resolveTableClient(client, ep, cfg.handshakeEndpoint, hSAS)
	if err != nil {
		return nil, err
	}
	tt, err := resolveTableClient(client, ep, cfg.tokenEndpoint, tSAS)
	if err != nil {
		return nil, err
	}

	return &tableDriver{
		ep:             ep,
		client:         client,
		cfg:            cfg,
		handshakeTable: ht,
		tokenTable:     tt,
	}, nil
}

func resolveTableClient(client *aztables.ServiceClient, ep *Endpoint, name, sasToken string) (*aztables.Client, error) {
	if client != nil && sasToken == "" {
		return client.NewClient(name), nil
	}
	c, err := aztables.NewClientWithNoCredential(ep.JoinURL(name, sasToken), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrClientCreationFailed, err)
	}
	return c, nil
}

type tableDriver struct {
	ep                         *Endpoint
	client                     *aztables.ServiceClient
	cfg                        *Config
	handshakeTable, tokenTable *aztables.Client
}

func (p *tableDriver) PostHandshake(ctx context.Context, connID string, msg []byte) error {
	data, _ := buildTableEntity(p.cfg.handshakeEndpoint, connID, msg)
	_, err := p.handshakeTable.AddEntity(ctx, data, nil)
	return err
}

func (p *tableDriver) GetHandshakes(ctx context.Context) ([]Handshake, error) {
	pager := p.handshakeTable.NewListEntitiesPager(&aztables.ListEntitiesOptions{Filter: to.Ptr("PartitionKey eq '" + p.cfg.handshakeEndpoint + "'")})
	var handshakes []Handshake
	for pager.More() {
		resp, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, e := range resp.Entities {
			var meta struct{ RowKey string }
			json.Unmarshal(e, &meta)
			handshakes = append(handshakes, Handshake{ID: meta.RowKey, Payload: extractTableData(e)})
		}
	}
	return handshakes, nil
}

func (p *tableDriver) DeleteHandshake(ctx context.Context, id string) error {
	_, err := p.handshakeTable.DeleteEntity(ctx, p.cfg.handshakeEndpoint, id, nil)
	return err
}

func (p *tableDriver) PostToken(ctx context.Context, connID string, msg []byte) error {
	edata, _ := buildTableEntity(p.cfg.tokenEndpoint, connID, msg)
	_, err := p.tokenTable.AddEntity(ctx, edata, nil)
	return err
}

func (p *tableDriver) GetToken(ctx context.Context, connID string) ([]byte, error) {
	resp, err := p.tokenTable.GetEntity(ctx, p.cfg.tokenEndpoint, connID, nil)
	if err != nil {
		if re, ok := err.(*azcore.ResponseError); ok && re.StatusCode == http.StatusNotFound {
			return nil, ErrNoData
		}
		return nil, err
	}
	return extractTableData(resp.Value), nil
}

func (p *tableDriver) DeleteToken(ctx context.Context, connID string) error {
	_, err := p.tokenTable.DeleteEntity(ctx, p.cfg.tokenEndpoint, connID, nil)
	return err
}

func (p *tableDriver) makeSAS(name string, permissions aztables.SASPermissions) (string, error) {
	start, end := p.cfg.SASTimes()
	sv := aztables.SASSignatureValues{Protocol: aztables.SASProtocolHTTPSandHTTP, TableName: name, Permissions: permissions.String(), StartTime: start, ExpiryTime: end}
	cred, err := aztables.NewSharedKeyCredential(p.ep.Account, p.ep.Key)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrClientCreationFailed, err)
	}
	sasToken, err := sv.Sign(cred)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(sasToken, "?"), nil
}

func (p *tableDriver) CreateBootstrapTokens() (string, string, error) {
	if p.ep.Account == "" || p.ep.Key == "" {
		return "", "", ErrSASGenerationFailed
	}
	hSAS, err := p.makeSAS(p.cfg.handshakeEndpoint, aztables.SASPermissions{Add: true})
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrSASGenerationFailed, err)
	}
	tSAS, err := p.makeSAS(p.cfg.tokenEndpoint, aztables.SASPermissions{Read: true})
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrSASGenerationFailed, err)
	}
	return hSAS, tSAS, nil
}

func (p *tableDriver) CreateSession(ctx context.Context, connID string) (SessionTokens, error) {
	name := p.cfg.reqPrefix + strings.ReplaceAll(connID, "-", "")
	resName := p.cfg.resPrefix + strings.ReplaceAll(connID, "-", "")
	if _, err := p.client.CreateTable(ctx, name, nil); err != nil {
		return SessionTokens{}, fmt.Errorf("create session table %s: %w", name, err)
	}
	if _, err := p.client.CreateTable(ctx, resName, nil); err != nil {
		return SessionTokens{}, fmt.Errorf("create session table %s: %w", resName, err)
	}
	reqSAS, err := p.makeSAS(name, aztables.SASPermissions{Add: true})
	if err != nil {
		return SessionTokens{}, fmt.Errorf("%w: %v", ErrSASGenerationFailed, err)
	}
	resSAS, err := p.makeSAS(resName, aztables.SASPermissions{Read: true, Delete: true})
	if err != nil {
		return SessionTokens{}, fmt.Errorf("%w: %v", ErrSASGenerationFailed, err)
	}
	return SessionTokens{Req: reqSAS, Res: resSAS}, nil
}

func (p *tableDriver) NewTransport(_ context.Context, connID string, tokens SessionTokens, isInitiator bool) (Transport, error) {
	sid := strings.ReplaceAll(connID, "-", "")
	reqName, resName := p.cfg.reqPrefix+sid, p.cfg.resPrefix+sid
	var tx, rx *aztables.Client
	if isInitiator {
		var err error
		tx, err = aztables.NewClientWithNoCredential(p.ep.JoinURL(reqName, tokens.Req), nil)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrClientCreationFailed, err)
		}
		rx, err = aztables.NewClientWithNoCredential(p.ep.JoinURL(resName, tokens.Res), nil)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrClientCreationFailed, err)
		}
	} else {
		tx, rx = p.client.NewClient(resName), p.client.NewClient(reqName)
	}
	return &tableTransport{connID: connID, txClient: tx, rxClient: rx, ep: p.ep, txName: reqName, rxName: resName, cfg: p.cfg}, nil
}

func (p *tableDriver) CleanupBootstrap(ctx context.Context) error {
	if p.client == nil {
		return nil
	}
	_, a := p.client.DeleteTable(ctx, p.cfg.handshakeEndpoint, nil)
	_, b := p.client.DeleteTable(ctx, p.cfg.tokenEndpoint, nil)
	return errors.Join(missingDelete(a), missingDelete(b))
}

func (p *tableDriver) CleanupSession(ctx context.Context, connID string) error {
	if p.client == nil {
		return nil
	}
	sid := strings.ReplaceAll(connID, "-", "")
	_, a := p.client.DeleteTable(ctx, p.cfg.reqPrefix+sid, nil)
	_, b := p.client.DeleteTable(ctx, p.cfg.resPrefix+sid, nil)
	return errors.Join(missingDelete(a), missingDelete(b))
}

type tableTransport struct {
	rxErr              error
	txClient, rxClient *aztables.Client
	ep                 *Endpoint
	cfg                *Config
	connID             string
	txName, rxName     string
	pending            []byte
	ends               []int
	rxSeq              int
	reclaimSeq         int
	reclaimEnd         int
	position           int
	txConfirmed        uint64
	mu                 sync.Mutex
	txMu               sync.Mutex
	active             bool
	txHasConfirmed     bool
	reclaimSingles     bool
}

// WriteRaw is serialized by Conn. Keep the confirmed high-water mark locally so
// an explicitly repeated older call cannot recreate a row already reclaimed by
// the receiver. An uncertain current write still retries AddEntity verbatim.
func (t *tableTransport) WriteRaw(ctx context.Context, seq uint64, data io.ReadSeeker) error {
	t.txMu.Lock()
	defer t.txMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.txHasConfirmed && seq <= t.txConfirmed {
		return nil
	}
	// Nine-digit row keys are the existing wire format. Never wrap or reorder.
	if seq >= 1_000_000_000 {
		return fmt.Errorf("table sequence exhausted: %w", ErrBufferLimit)
	}
	raw, err := io.ReadAll(io.LimitReader(data, MaxTableEntitySize+1))
	if err != nil {
		return fmt.Errorf("read table write: %w", err)
	}
	if len(raw) > MaxTableEntitySize {
		return ErrBufferLimit
	}
	edata, err := buildTableEntity("data", formatRowKey(int(seq)), raw)
	if err != nil {
		return err
	}
	_, err = t.txClient.AddEntity(ctx, edata, nil)
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) && respErr.ErrorCode == "EntityAlreadyExists" {
		err = nil
	}
	if err == nil {
		t.txConfirmed, t.txHasConfirmed = seq, true
	}
	return err
}

func (t *tableTransport) ReadRaw(ctx context.Context) (io.ReadCloser, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.rxErr != nil {
		return nil, t.rxErr
	}
	if t.active {
		return nil, errors.New("table response still open")
	}
	if t.position < len(t.pending) {
		t.active = true
		return &tableBody{t: t, ctx: ctx}, nil
	}
	// Consumption alone is insufficient: keep the newest row as the retry
	// receipt. Consuming its successor proves the serialized writer advanced
	// beyond it, so that row can no longer be the uncertain outgoing chunk.
	// Start only at the threshold. Once started, finish the captured range even
	// if a partial failure leaves fewer rows than the threshold. A failed delete
	// keeps its cursor; 404 reconciles an uncertain successful delete on retry.
	if t.reclaimSeq == t.reclaimEnd && t.rxSeq-1-t.reclaimSeq >= tableCleanupRows {
		t.reclaimEnd = t.rxSeq - 1
	}
	for t.reclaimSeq < t.reclaimEnd && !t.reclaimSingles {
		// Deletes contain only keys. The normal range is at most the threshold
		// plus one prefetch page, comfortably below Azure's 100-action/4 MiB cap.
		end := min(t.reclaimEnd, t.reclaimSeq+100)
		actions := make([]aztables.TransactionAction, 0, end-t.reclaimSeq)
		for seq := t.reclaimSeq; seq < end; seq++ {
			entity, _ := buildTableEntity("data", formatRowKey(seq), nil)
			actions = append(actions, aztables.TransactionAction{ActionType: aztables.TransactionTypeDelete, Entity: entity})
		}
		if _, err := t.rxClient.SubmitTransaction(ctx, actions, nil); err != nil {
			// A missing row rejects the entire atomic batch. A lost response may
			// instead mean every delete committed. Surface the error now; the next
			// call reconciles each row (404 included) without guessing the outcome.
			t.reclaimSingles = true
			return nil, fmt.Errorf("reclaim table batch [%d,%d): %w", t.reclaimSeq, end, err)
		}
		t.reclaimSeq = end
	}
	for t.reclaimSeq < t.reclaimEnd {
		_, err := t.rxClient.DeleteEntity(ctx, "data", formatRowKey(t.reclaimSeq), nil)
		if err = missingDelete(err); err != nil {
			return nil, fmt.Errorf("reclaim table row %d: %w", t.reclaimSeq, err)
		}
		t.reclaimSeq++
	}
	t.reclaimSingles = false
	if t.rxSeq >= 1_000_000_000 {
		return nil, fmt.Errorf("table receive sequence exhausted: %w", ErrBufferLimit)
	}
	rows := min(tableReadRows, max(1, t.cfg.limits().Pending/MaxTableEntitySize))
	pager := t.rxClient.NewListEntitiesPager(&aztables.ListEntitiesOptions{Filter: to.Ptr("PartitionKey eq 'data' and RowKey ge '" + formatRowKey(t.rxSeq) + "'"), Top: to.Ptr(int32(rows))})
	if !pager.More() {
		return nil, ErrNoData
	}
	resp, err := pager.NextPage(ctx)
	if err != nil {
		return nil, fmt.Errorf("read table entities: %w", err)
	}
	t.pending, t.ends, t.position = nil, nil, 0
	for _, e := range resp.Entities {
		var meta struct{ RowKey string }
		if err := json.Unmarshal(e, &meta); err != nil {
			return nil, fmt.Errorf("decode table row: %w", err)
		}
		if meta.RowKey != formatRowKey(t.rxSeq+len(t.ends)) {
			break
		}
		data, err := extractSessionTableData(e, t.cfg.limits().Pending-len(t.pending))
		if err != nil || len(t.ends) >= rows {
			t.rxErr = fmt.Errorf("%w: table receive page: %v", ErrBufferLimit, err)
			t.pending, t.ends = nil, nil
			return nil, t.rxErr
		}
		t.pending = append(t.pending, data...)
		t.ends = append(t.ends, len(t.pending))
	}
	if len(t.pending) == 0 {
		return nil, ErrNoData
	}
	t.active = true
	return &tableBody{t: t, ctx: ctx}, nil
}

// tableBody transfers ownership incrementally. Closing early preserves the
// unread suffix, and fetching never advances consumption or deletes rows.
type tableBody struct {
	t      *tableTransport
	ctx    context.Context
	closed bool
}

func (b *tableBody) Read(p []byte) (int, error) {
	b.t.mu.Lock()
	defer b.t.mu.Unlock()
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	n := copy(p, b.t.pending[b.t.position:])
	b.t.position += n
	for len(b.t.ends) > 0 && b.t.position >= b.t.ends[0] {
		b.t.rxSeq++
		b.t.ends = b.t.ends[1:]
	}
	if b.t.position == len(b.t.pending) {
		return n, io.EOF
	}
	return n, nil
}

func (b *tableBody) Close() error {
	b.t.mu.Lock()
	defer b.t.mu.Unlock()
	if !b.closed {
		b.closed = true
		b.t.active = false
		if b.t.position == len(b.t.pending) {
			b.t.pending, b.t.ends, b.t.position = nil, nil, 0
		}
	}
	return nil
}

func (t *tableTransport) Close() error    { return nil }
func (t *tableTransport) MaxRawSize() int { return MaxTableEntitySize }
func (t *tableTransport) LocalAddr() net.Addr {
	return ServiceAddr{tableDriverName, t.ep.ServiceURL(), t.txName}
}
func (t *tableTransport) RemoteAddr() net.Addr {
	return ServiceAddr{tableDriverName, t.ep.ServiceURL(), t.rxName}
}

func formatRowKey(seq int) string {
	var b [9]byte
	for i := 8; i >= 0; i-- {
		b[i] = byte('0' + (seq % 10))
		seq /= 10
	}
	return string(b[:])
}

func newTableClient(ep *Endpoint) (*aztables.ServiceClient, error) {
	if ep.Account != "" && ep.Key != "" {
		cred, err := aztables.NewSharedKeyCredential(ep.Account, ep.Key)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrClientCreationFailed, err)
		}
		return aztables.NewServiceClientWithSharedKey(ep.ServiceURL(), cred, nil)
	}
	return nil, nil
}
