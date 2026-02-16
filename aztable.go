package aznet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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

type tableFactory struct{}

func (d *tableFactory) NewDriver(ep *Endpoint, cfg *Config) (Driver, error) {
	client, err := newTableClient(ep)
	if err != nil {
		return nil, err
	}
	if client != nil {
		for _, name := range []string{cfg.handshakeEndpoint, cfg.tokenEndpoint} {
			_, _ = client.CreateTable(cfg.ctx, name, nil)
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
	resSAS, err := p.makeSAS(resName, aztables.SASPermissions{Read: true})
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
	_, _ = p.client.DeleteTable(ctx, p.cfg.handshakeEndpoint, nil)
	_, _ = p.client.DeleteTable(ctx, p.cfg.tokenEndpoint, nil)
	return nil
}

func (p *tableDriver) CleanupSession(ctx context.Context, connID string) error {
	if p.client == nil {
		return nil
	}
	sid := strings.ReplaceAll(connID, "-", "")
	_, _ = p.client.DeleteTable(ctx, p.cfg.reqPrefix+sid, nil)
	_, _ = p.client.DeleteTable(ctx, p.cfg.resPrefix+sid, nil)
	return nil
}

type tableTransport struct {
	txClient, rxClient *aztables.Client
	ep                 *Endpoint
	cfg                *Config

	connID         string
	txName, rxName string
	mu             sync.Mutex
	txSeq, rxSeq   int
}

func (t *tableTransport) WriteRaw(ctx context.Context, data io.ReadSeeker) error {
	t.mu.Lock()
	seq := t.txSeq
	t.txSeq++
	t.mu.Unlock()
	raw, _ := io.ReadAll(data)
	edata, _ := buildTableEntity("data", formatRowKey(seq), raw)
	_, err := t.txClient.AddEntity(ctx, edata, nil)
	return err
}

func (t *tableTransport) ReadRaw(ctx context.Context) (io.ReadCloser, error) {
	t.mu.Lock()
	seq := t.rxSeq
	t.mu.Unlock()
	pager := t.rxClient.NewListEntitiesPager(&aztables.ListEntitiesOptions{Filter: to.Ptr("PartitionKey eq 'data' and RowKey ge '" + formatRowKey(seq) + "'"), Top: to.Ptr(int32(10))})
	if pager.More() {
		resp, err := pager.NextPage(ctx)
		if err == nil && len(resp.Entities) > 0 {
			var combined bytes.Buffer
			processed := 0
			for _, e := range resp.Entities {
				var meta struct{ RowKey string }
				json.Unmarshal(e, &meta)
				if meta.RowKey != formatRowKey(seq+processed) {
					break
				}
				combined.Write(extractTableData(e))
				processed++
			}
			if processed > 0 {
				t.mu.Lock()
				t.rxSeq += processed
				t.mu.Unlock()
				return io.NopCloser(bytes.NewReader(combined.Bytes())), nil
			}
		}
	}
	return nil, ErrNoData
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
