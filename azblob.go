package aznet

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"
)

const blobDriverName = "azblob"

// MaxBlobBlockSize is the maximum size of a single block in an Append Blob (4 MB).
const MaxBlobBlockSize = 4 * 1024 * 1024

// MaxBlocksPerBlob is the maximum number of blocks per append blob.
const MaxBlocksPerBlob = 50000

func init() {
	RegisterFactory(blobDriverName, &blobFactory{})
}

type blobFactory struct{}

func (d *blobFactory) NewDriver(ep *Endpoint, cfg *Config) (Driver, error) {
	client, err := newBlobClient(ep)
	if err != nil {
		return nil, err
	}

	if client != nil {
		for _, name := range []string{cfg.handshakeEndpoint, cfg.tokenEndpoint} {
			if _, err := client.CreateContainer(cfg.ctx, name, nil); err != nil && !bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
				return nil, err
			}
		}
	}

	var hSAS, tSAS string
	if client == nil {
		hSAS, tSAS, _ = ep.ParseSAS(cfg)
	}

	hc, err := resolveContainerClient(client, ep, cfg.handshakeEndpoint, hSAS)
	if err != nil {
		return nil, err
	}
	tc, err := resolveContainerClient(client, ep, cfg.tokenEndpoint, tSAS)
	if err != nil {
		return nil, err
	}

	return &blobDriver{
		ep:                 ep,
		client:             client,
		cfg:                cfg,
		handshakeContainer: hc,
		tokenContainer:     tc,
	}, nil
}

func resolveContainerClient(client *service.Client, ep *Endpoint, name, sasToken string) (*container.Client, error) {
	if client != nil && sasToken == "" {
		return client.NewContainerClient(name), nil
	}
	c, err := container.NewClientWithNoCredential(ep.JoinURL(name, sasToken), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrClientCreationFailed, err)
	}
	return c, nil
}

type blobDriver struct {
	ep     *Endpoint
	client *service.Client
	cfg    *Config

	handshakeContainer, tokenContainer *container.Client
}

func (p *blobDriver) PostHandshake(ctx context.Context, connID string, msg []byte) error {
	encoded := base64.StdEncoding.EncodeToString(msg)
	_, err := p.handshakeContainer.NewBlockBlobClient(connID).Upload(ctx,
		streaming.NopCloser(bytes.NewReader(nil)),
		&blockblob.UploadOptions{Metadata: map[string]*string{"payload": &encoded}},
	)
	return err
}

// GetHandshakes lists blobs with metadata included, avoiding per-blob downloads (1 API call instead of N+1).
func (p *blobDriver) GetHandshakes(ctx context.Context) ([]Handshake, error) {
	pager := p.handshakeContainer.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
		Include: container.ListBlobsInclude{Metadata: true},
	})
	var handshakes []Handshake
	for pager.More() {
		resp, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, item := range resp.Segment.BlobItems {
			if item.Name == nil || item.Metadata == nil {
				continue
			}
			encoded, ok := item.Metadata["payload"]
			if !ok || encoded == nil {
				continue
			}
			data, err := base64.StdEncoding.DecodeString(*encoded)
			if err != nil {
				continue
			}
			handshakes = append(handshakes, Handshake{ID: *item.Name, Payload: data})
		}
	}
	return handshakes, nil
}

func (p *blobDriver) DeleteHandshake(ctx context.Context, id string) error {
	_, err := p.handshakeContainer.NewBlobClient(id).Delete(ctx, nil)
	return err
}

func (p *blobDriver) PostToken(ctx context.Context, connID string, msg []byte) error {
	_, err := p.tokenContainer.NewBlockBlobClient(connID).Upload(ctx, streaming.NopCloser(bytes.NewReader(msg)), nil)
	return err
}

func (p *blobDriver) GetToken(ctx context.Context, connID string) ([]byte, error) {
	resp, err := p.tokenContainer.NewBlobClient(connID).DownloadStream(ctx, nil)
	if err != nil {
		if bloberror.HasCode(err, bloberror.BlobNotFound) {
			return nil, ErrNoData
		}
		return nil, err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if len(data) == 0 {
		return nil, ErrNoData
	}
	return data, nil
}

func (p *blobDriver) DeleteToken(ctx context.Context, connID string) error {
	_, err := p.tokenContainer.NewBlobClient(connID).Delete(ctx, nil)
	return err
}

func (p *blobDriver) makeSAS(name string, permissions sas.ContainerPermissions) (string, error) {
	start, end := p.cfg.SASTimes()
	sv := sas.BlobSignatureValues{
		Protocol: sas.ProtocolHTTPSandHTTP, ContainerName: name,
		Permissions: permissions.String(), StartTime: start, ExpiryTime: end,
	}

	cred, err := azblob.NewSharedKeyCredential(p.ep.Account, p.ep.Key)
	if err != nil {
		return "", err
	}
	sasToken, err := sv.SignWithSharedKey(cred)
	if err != nil {
		return "", err
	}

	return strings.TrimPrefix(sasToken.Encode(), "?"), nil
}

func (p *blobDriver) CreateBootstrapTokens() (string, string, error) {
	if p.ep.Account == "" || p.ep.Key == "" {
		return "", "", ErrSASGenerationFailed
	}

	hSAS, err := p.makeSAS(p.cfg.handshakeEndpoint, sas.ContainerPermissions{Add: true, Create: true, Write: true})
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrSASGenerationFailed, err)
	}
	tSAS, err := p.makeSAS(p.cfg.tokenEndpoint, sas.ContainerPermissions{Read: true, List: true})
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrSASGenerationFailed, err)
	}

	return hSAS, tSAS, nil
}

func (p *blobDriver) CreateSession(ctx context.Context, connID string) (SessionTokens, error) {
	if _, err := p.client.CreateContainer(ctx, connID, nil); err != nil && !bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
		return SessionTokens{}, fmt.Errorf("create session container: %w", err)
	}
	tokenSAS, err := p.makeSAS(connID, sas.ContainerPermissions{Read: true, List: true, Add: true, Create: true, Write: true})
	if err != nil {
		return SessionTokens{}, fmt.Errorf("%w: %v", ErrSASGenerationFailed, err)
	}
	return SessionTokens{Req: tokenSAS, Res: tokenSAS}, nil
}

func (p *blobDriver) NewTransport(ctx context.Context, connID string, tokens SessionTokens, isInitiator bool) (Transport, error) {
	client, err := service.NewClientWithNoCredential(p.ep.JoinURL("", tokens.Req), nil)
	if err != nil {
		return nil, err
	}
	t := &blobTransport{
		connID: connID, containerClient: client.NewContainerClient(connID),
		cfg: p.cfg, ep: p.ep, isInitiator: isInitiator,
	}
	if isInitiator {
		t.txBlob, t.rxBlob = p.cfg.reqPrefix+"-0", p.cfg.resPrefix+"-0"
	} else {
		t.txBlob, t.rxBlob = p.cfg.resPrefix+"-0", p.cfg.reqPrefix+"-0"
		if _, err := t.containerClient.NewAppendBlobClient(t.txBlob).Create(ctx, nil); err != nil {
			return nil, fmt.Errorf("create tx blob: %w", err)
		}
		if _, err := t.containerClient.NewAppendBlobClient(t.rxBlob).Create(ctx, nil); err != nil {
			return nil, fmt.Errorf("create rx blob: %w", err)
		}
	}
	return t, nil
}

func (p *blobDriver) CleanupBootstrap(ctx context.Context) error {
	if p.client == nil {
		return nil
	}
	_, _ = p.client.NewContainerClient(p.cfg.handshakeEndpoint).Delete(ctx, nil)
	_, _ = p.client.NewContainerClient(p.cfg.tokenEndpoint).Delete(ctx, nil)
	return nil
}

func (p *blobDriver) CleanupSession(ctx context.Context, connID string) error {
	if p.client == nil {
		return nil
	}
	_, _ = p.client.NewContainerClient(connID).Delete(ctx, nil)
	return nil
}

type blobTransport struct {
	containerClient *container.Client
	cfg             *Config
	ep              *Endpoint

	connID         string
	txBlob, rxBlob string
	blocksWritten  int64
	readOffset     int64
	txSeq, rxSeq   int
	mu             sync.Mutex
	isInitiator    bool
}

func (t *blobTransport) WriteRaw(ctx context.Context, data io.ReadSeeker) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, err := t.containerClient.NewAppendBlobClient(t.txBlob).AppendBlock(ctx, streaming.NopCloser(data), nil)
	if err == nil {
		t.blocksWritten++
	}
	return err
}

func (t *blobTransport) ReadRaw(ctx context.Context) (io.ReadCloser, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	resp, err := t.containerClient.NewBlobClient(t.rxBlob).DownloadStream(ctx, &blob.DownloadStreamOptions{Range: blob.HTTPRange{Offset: t.readOffset}})
	if err != nil {
		if re, ok := err.(*azcore.ResponseError); ok && (re.StatusCode == http.StatusNotFound || re.StatusCode == http.StatusRequestedRangeNotSatisfiable) {
			return nil, ErrNoData
		}
		return nil, err
	}
	contentLen := int64(0)
	if resp.ContentLength != nil {
		contentLen = *resp.ContentLength
	}
	if contentLen == 0 {
		resp.Body.Close()
		return nil, ErrNoData
	}
	t.readOffset += contentLen
	return resp.Body, nil
}

func (t *blobTransport) Close() error    { return nil }
func (t *blobTransport) MaxRawSize() int { return MaxBlobBlockSize }
func (t *blobTransport) LocalAddr() net.Addr {
	return ServiceAddr{blobDriverName, t.ep.ServiceURL(), t.connID + "/" + t.rxBlob}
}
func (t *blobTransport) RemoteAddr() net.Addr {
	return ServiceAddr{blobDriverName, t.ep.ServiceURL(), t.connID + "/" + t.txBlob}
}

func (t *blobTransport) ShouldRotate() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.blocksWritten >= MaxBlocksPerBlob-10
}

func (t *blobTransport) RotateTX(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.txSeq++
	prefix := t.cfg.reqPrefix
	if !t.isInitiator {
		prefix = t.cfg.resPrefix
	}
	t.txBlob = prefix + "-" + strconv.Itoa(t.txSeq)
	t.blocksWritten = 0
	_, err := t.containerClient.NewAppendBlobClient(t.txBlob).Create(ctx, nil)
	return err
}

func (t *blobTransport) RotateRX() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rxSeq++
	prefix := t.cfg.resPrefix
	if !t.isInitiator {
		prefix = t.cfg.reqPrefix
	}
	t.rxBlob = prefix + "-" + strconv.Itoa(t.rxSeq)
	t.readOffset = 0
	return nil
}

func newBlobClient(ep *Endpoint) (*service.Client, error) {
	if ep.Account != "" && ep.Key != "" {
		cred, err := azblob.NewSharedKeyCredential(ep.Account, ep.Key)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrClientCreationFailed, err)
		}
		c, err := azblob.NewClientWithSharedKeyCredential(ep.ServiceURL(), cred, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrClientCreationFailed, err)
		}
		return c.ServiceClient(), nil
	}
	return nil, nil
}
