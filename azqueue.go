package aznet

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azqueue"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azqueue/queueerror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azqueue/sas"
)

const queueDriverName = "azqueue"

// MaxQueuePayload is the maximum raw data size stored in a queue message (64 KB).
const MaxQueueTextMessageSize = 64 * 1024

func init() {
	RegisterFactory(queueDriverName, &queueFactory{})
}

type queueFactory struct{}

func (d *queueFactory) NewDriver(ep *Endpoint, cfg *Config) (Driver, error) {
	client, err := newQueueClient(ep)
	if err != nil {
		return nil, err
	}

	if client != nil {
		for _, name := range []string{cfg.handshakeEndpoint, cfg.tokenEndpoint} {
			if _, err := client.CreateQueue(cfg.ctx, name, nil); err != nil && !queueerror.HasCode(err, queueerror.QueueAlreadyExists) {
				return nil, err
			}
		}
	}

	var hSAS, tSAS string
	if client == nil {
		hSAS, tSAS, _ = ep.ParseSAS(cfg)
	}

	hq, err := resolveQueueClient(client, ep, cfg.handshakeEndpoint, hSAS)
	if err != nil {
		return nil, err
	}
	tq, err := resolveQueueClient(client, ep, cfg.tokenEndpoint, tSAS)
	if err != nil {
		return nil, err
	}

	return &queueDriver{
		ep:             ep,
		client:         client,
		cfg:            cfg,
		handshakeQueue: hq,
		tokenQueue:     tq,
	}, nil
}

func resolveQueueClient(client *azqueue.ServiceClient, ep *Endpoint, name, sasToken string) (*azqueue.QueueClient, error) {
	if client != nil && sasToken == "" {
		return client.NewQueueClient(name), nil
	}
	c, err := azqueue.NewQueueClientWithNoCredential(ep.JoinURL(name, sasToken), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrClientCreationFailed, err)
	}
	return c, nil
}

type queueDriver struct {
	ep     *Endpoint
	client *azqueue.ServiceClient
	cfg    *Config

	handshakeQueue, tokenQueue *azqueue.QueueClient
	receipts                   sync.Map // connID -> messageID:popReceipt
}

func (p *queueDriver) PostHandshake(ctx context.Context, connID string, msg []byte) error {
	_, err := p.handshakeQueue.EnqueueMessage(ctx, base64.StdEncoding.EncodeToString(msg), nil)
	return err
}

func (p *queueDriver) GetHandshakes(ctx context.Context) ([]Handshake, error) {
	resp, err := p.handshakeQueue.DequeueMessages(ctx, &azqueue.DequeueMessagesOptions{NumberOfMessages: to.Ptr[int32](32), VisibilityTimeout: to.Ptr[int32](60)})
	if err != nil {
		return nil, err
	}
	var handshakes []Handshake
	for _, msg := range resp.Messages {
		if msg.MessageText != nil {
			data, _ := base64.StdEncoding.DecodeString(*msg.MessageText)
			handshakes = append(handshakes, Handshake{ID: *msg.MessageID + ":" + *msg.PopReceipt, Payload: data})
		}
	}
	return handshakes, nil
}

func (p *queueDriver) DeleteHandshake(ctx context.Context, id string) error {
	parts := strings.Split(id, ":")
	if len(parts) != 2 {
		return fmt.Errorf("invalid handshake id format")
	}
	_, err := p.handshakeQueue.DeleteMessage(ctx, parts[0], parts[1], nil)
	return err
}

func (p *queueDriver) PostToken(ctx context.Context, connID string, msg []byte) error {
	txt := connID + ":" + base64.StdEncoding.EncodeToString(msg)
	resp, err := p.tokenQueue.EnqueueMessage(ctx, txt, nil)
	if err == nil && len(resp.Messages) > 0 {
		p.receipts.Store(connID, *resp.Messages[0].MessageID+":"+*resp.Messages[0].PopReceipt)
	}
	return err
}

func (p *queueDriver) GetToken(ctx context.Context, connID string) ([]byte, error) {
	resp, err := p.tokenQueue.PeekMessages(ctx, &azqueue.PeekMessagesOptions{NumberOfMessages: to.Ptr[int32](32)})
	if err != nil {
		return nil, err
	}
	for _, msg := range resp.Messages {
		if msg.MessageText != nil && strings.HasPrefix(*msg.MessageText, connID+":") {
			return base64.StdEncoding.DecodeString(strings.TrimPrefix(*msg.MessageText, connID+":"))
		}
	}
	return nil, ErrNoData
}

func (p *queueDriver) DeleteToken(ctx context.Context, connID string) error {
	if val, ok := p.receipts.LoadAndDelete(connID); ok {
		parts := strings.Split(val.(string), ":")
		_, err := p.tokenQueue.DeleteMessage(ctx, parts[0], parts[1], nil)
		return err
	}
	return nil
}

func (p *queueDriver) makeSAS(name string, permissions sas.QueuePermissions) (string, error) {
	start, end := p.cfg.SASTimes()
	sv := sas.QueueSignatureValues{Protocol: sas.ProtocolHTTPSandHTTP, QueueName: name, Permissions: permissions.String(), StartTime: start, ExpiryTime: end}
	cred, err := azqueue.NewSharedKeyCredential(p.ep.Account, p.ep.Key)
	if err != nil {
		return "", err
	}
	sasToken, err := sv.SignWithSharedKey(cred)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(sasToken.Encode(), "?"), nil
}

func (p *queueDriver) CreateBootstrapTokens() (string, string, error) {
	if p.ep.Account == "" || p.ep.Key == "" {
		return "", "", ErrSASGenerationFailed
	}
	hSAS, err := p.makeSAS(p.cfg.handshakeEndpoint, sas.QueuePermissions{Add: true})
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrSASGenerationFailed, err)
	}
	tSAS, err := p.makeSAS(p.cfg.tokenEndpoint, sas.QueuePermissions{Read: true})
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrSASGenerationFailed, err)
	}
	return hSAS, tSAS, nil
}

func (p *queueDriver) CreateSession(ctx context.Context, connID string) (SessionTokens, error) {
	reqName, resName := p.cfg.reqPrefix+"-"+connID, p.cfg.resPrefix+"-"+connID
	if _, err := p.client.CreateQueue(ctx, reqName, nil); err != nil && !queueerror.HasCode(err, queueerror.QueueAlreadyExists) {
		return SessionTokens{}, fmt.Errorf("create session queue %s: %w", reqName, err)
	}
	if _, err := p.client.CreateQueue(ctx, resName, nil); err != nil && !queueerror.HasCode(err, queueerror.QueueAlreadyExists) {
		return SessionTokens{}, fmt.Errorf("create session queue %s: %w", resName, err)
	}
	reqSAS, err := p.makeSAS(reqName, sas.QueuePermissions{Add: true})
	if err != nil {
		return SessionTokens{}, fmt.Errorf("%w: %v", ErrSASGenerationFailed, err)
	}
	resSAS, err := p.makeSAS(resName, sas.QueuePermissions{Read: true, Process: true})
	if err != nil {
		return SessionTokens{}, fmt.Errorf("%w: %v", ErrSASGenerationFailed, err)
	}
	return SessionTokens{Req: reqSAS, Res: resSAS}, nil
}

func (p *queueDriver) NewTransport(_ context.Context, connID string, tokens SessionTokens, isInitiator bool) (Transport, error) {
	reqName, resName := p.cfg.reqPrefix+"-"+connID, p.cfg.resPrefix+"-"+connID
	var tx, rx *azqueue.QueueClient
	if isInitiator {
		var err error
		tx, err = azqueue.NewQueueClientWithNoCredential(p.ep.JoinURL(reqName, tokens.Req), nil)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrClientCreationFailed, err)
		}
		rx, err = azqueue.NewQueueClientWithNoCredential(p.ep.JoinURL(resName, tokens.Res), nil)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrClientCreationFailed, err)
		}
	} else {
		tx, rx = p.client.NewQueueClient(resName), p.client.NewQueueClient(reqName)
	}
	return &queueTransport{connID: connID, txQueue: tx, rxQueue: rx, ep: p.ep, txName: reqName, rxName: resName, cfg: p.cfg}, nil
}

func (p *queueDriver) CleanupBootstrap(ctx context.Context) error {
	if p.client == nil {
		return nil
	}
	_, _ = p.client.NewQueueClient(p.cfg.handshakeEndpoint).Delete(ctx, nil)
	_, _ = p.client.NewQueueClient(p.cfg.tokenEndpoint).Delete(ctx, nil)
	return nil
}

func (p *queueDriver) CleanupSession(ctx context.Context, connID string) error {
	if p.client == nil {
		return nil
	}
	_, _ = p.client.NewQueueClient(p.cfg.reqPrefix+"-"+connID).Delete(ctx, nil)
	_, _ = p.client.NewQueueClient(p.cfg.resPrefix+"-"+connID).Delete(ctx, nil)
	return nil
}

type queueTransport struct {
	txQueue, rxQueue *azqueue.QueueClient
	ep               *Endpoint
	cfg              *Config

	connID         string
	txName, rxName string
}

func (t *queueTransport) WriteRaw(ctx context.Context, data io.ReadSeeker) error {
	raw, _ := io.ReadAll(data)
	_, err := t.txQueue.EnqueueMessage(ctx, base64.StdEncoding.EncodeToString(raw), nil)
	return err
}

func (t *queueTransport) ReadRaw(ctx context.Context) (io.ReadCloser, error) {
	resp, err := t.rxQueue.DequeueMessages(ctx, &azqueue.DequeueMessagesOptions{NumberOfMessages: to.Ptr[int32](32)})
	if err != nil || len(resp.Messages) == 0 {
		return nil, ErrNoData
	}
	var combined []byte
	for _, msg := range resp.Messages {
		if msg.MessageText != nil {
			data, _ := base64.StdEncoding.DecodeString(*msg.MessageText)
			combined = append(combined, data...)
			_, _ = t.rxQueue.DeleteMessage(ctx, *msg.MessageID, *msg.PopReceipt, nil)
		}
	}
	if len(combined) == 0 {
		return nil, ErrNoData
	}
	return io.NopCloser(bytes.NewReader(combined)), nil
}

func (t *queueTransport) Close() error    { return nil }
func (t *queueTransport) MaxRawSize() int { return (MaxQueueTextMessageSize * 3) / 4 }
func (t *queueTransport) LocalAddr() net.Addr {
	return ServiceAddr{queueDriverName, t.ep.ServiceURL(), t.txName}
}
func (t *queueTransport) RemoteAddr() net.Addr {
	return ServiceAddr{queueDriverName, t.ep.ServiceURL(), t.rxName}
}

func newQueueClient(ep *Endpoint) (*azqueue.ServiceClient, error) {
	if ep.Account != "" && ep.Key != "" {
		cred, err := azqueue.NewSharedKeyCredential(ep.Account, ep.Key)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrClientCreationFailed, err)
		}
		return azqueue.NewServiceClientWithSharedKeyCredential(ep.ServiceURL(), cred, nil)
	}
	return nil, nil
}
