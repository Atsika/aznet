package aznet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azqueue"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azqueue/queueerror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azqueue/sas"
)

const queueDriverName = "azqueue"

// MaxQueuePayload is the maximum raw data size stored in a queue message (64 KB).
const MaxQueueTextMessageSize = 64 * 1024

const (
	// seqHeaderSize is the big-endian sequence number prepended to every queue
	// message. Azure Storage Queues are at-least-once and not strictly FIFO, so
	// the receiver uses this to dedup and reorder into a clean byte stream.
	seqHeaderSize = 8
	// queueSizeMargin keeps the base64-encoded message safely below the 64 KiB
	// queue ceiling. See MaxRawSize: base64 expands by 4/3, and the encoded form
	// of (seqHeaderSize + sealed chunk) must stay <= MaxQueueTextMessageSize.
	queueSizeMargin = 64
	// maxPendingMessages caps the reassembly buffer's memory footprint. Liveness
	// is not tied to it: the stall clock in ReadRaw fails a missing sequence.
	maxPendingMessages = 256
	// defaultReassemblyStall bounds the wait for a missing sequence when the
	// config carries no usable idle timeout.
	defaultReassemblyStall = 60 * time.Second
)

// ErrReassemblyOverflow is returned when the azqueue reassembly buffer exceeds
// maxPendingMessages, indicating a sequence gap that will not resolve.
var ErrReassemblyOverflow = errors.New("azqueue: reassembly buffer overflow")

// ErrReassemblyStalled is returned when a sequence gap in the azqueue receive
// stream fails to close within the stall bound, i.e. a message was lost and the
// byte stream can never be completed.
var ErrReassemblyStalled = errors.New("azqueue: reassembly stalled on missing sequence")

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
	return &queueTransport{connID: connID, txQueue: tx, rxQueue: rx, ep: p.ep, txName: reqName, rxName: resName, cfg: p.cfg, pending: make(map[uint64][]byte)}, nil
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

	// rmu guards the reassembly state below. Conn.Read calls ReadRaw with its own
	// lock released, so concurrent readers would otherwise race the pending map.
	// Never held across a queue round-trip.
	rmu     sync.Mutex
	pending map[uint64][]byte // rx reassembly buffer

	// rxErr is the sticky reassembly failure: overflow and stall both mean a gap
	// that will never close, so every later ReadRaw repeats the same verdict
	// rather than letting retries grow pending without bound.
	rxErr error

	// stallSince is when rxSeq became blocked on a missing sequence (zero if not).
	// Placed among the pointer fields so time.Time's *Location does not extend the
	// struct's pointer-scan region.
	stallSince time.Time

	connID         string
	txName, rxName string

	// txSeq needs no lock: WriteRaw is only reached via Conn.flush, which holds fmu.
	txSeq uint64 // next sequence to send
	rxSeq uint64 // next contiguous sequence expected
}

// encodeQueueMessage prepends the big-endian sequence to raw and base64-encodes
// the result for transport in a single queue message.
func encodeQueueMessage(seq uint64, raw []byte) string {
	msg := make([]byte, seqHeaderSize+len(raw))
	binary.BigEndian.PutUint64(msg[:seqHeaderSize], seq)
	copy(msg[seqHeaderSize:], raw)
	return base64.StdEncoding.EncodeToString(msg)
}

// decodeQueueMessage reverses encodeQueueMessage.
func decodeQueueMessage(text string) (uint64, []byte, error) {
	data, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return 0, nil, err
	}
	if len(data) < seqHeaderSize {
		return 0, nil, fmt.Errorf("azqueue: message too short (%d bytes)", len(data))
	}
	return binary.BigEndian.Uint64(data[:seqHeaderSize]), data[seqHeaderSize:], nil
}

// ingestLocked buffers one decoded message, discarding duplicates (already-
// delivered or already-buffered sequences). It reports whether the buffer has
// overflowed. Caller must hold t.rmu.
func (t *queueTransport) ingestLocked(seq uint64, payload []byte) (overflow bool) {
	if seq < t.rxSeq {
		return false // already delivered; redelivery under at-least-once
	}
	if _, dup := t.pending[seq]; dup {
		return false
	}
	t.pending[seq] = payload
	return len(t.pending) > maxPendingMessages
}

// drainLocked returns the contiguous in-order run starting at rxSeq, advancing
// rxSeq and removing the drained entries from the buffer. Caller must hold
// t.rmu.
func (t *queueTransport) drainLocked() []byte {
	var out []byte
	for {
		payload, ok := t.pending[t.rxSeq]
		if !ok {
			break
		}
		out = append(out, payload...)
		delete(t.pending, t.rxSeq)
		t.rxSeq++
	}
	return out
}

// stallLimit is how long rxSeq may stay blocked on a missing sequence before the
// connection fails. Tied to idleTimeout: past that the peer is already treated as
// dead (the listener's janitor reaps the session on the same clock), so there is
// no separate knob.
func (t *queueTransport) stallLimit() time.Duration {
	if t.cfg != nil && t.cfg.idleTimeout > 0 {
		return t.cfg.idleTimeout
	}
	return defaultReassemblyStall
}

func (t *queueTransport) WriteRaw(ctx context.Context, data io.ReadSeeker) error {
	raw, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	if _, err := t.txQueue.EnqueueMessage(ctx, encodeQueueMessage(t.txSeq, raw), nil); err != nil {
		return err
	}
	t.txSeq++ // only advance on success so sequences stay gap-free
	return nil
}

func (t *queueTransport) ReadRaw(ctx context.Context) (io.ReadCloser, error) {
	// Once reassembly has failed there is no path back to a correct byte stream,
	// so short-circuit before touching the queue and repeat the same verdict.
	t.rmu.Lock()
	failed := t.rxErr
	t.rmu.Unlock()
	if failed != nil {
		return nil, failed
	}

	resp, derr := t.rxQueue.DequeueMessages(ctx, &azqueue.DequeueMessagesOptions{NumberOfMessages: to.Ptr[int32](32)})

	overflow := false
	if derr == nil && len(resp.Messages) > 0 {
		var wg sync.WaitGroup

		t.rmu.Lock()
		for _, msg := range resp.Messages {
			if msg.MessageText == nil {
				continue
			}

			seq, payload, derr := decodeQueueMessage(*msg.MessageText)
			if derr != nil {
				// Leave it queued: deleting a sequence we never ingested opens
				// a gap that can never close. It reappears after the timeout.
				continue
			}
			overflow = t.ingestLocked(seq, payload)

			// Delete only now the message is accounted for; redeliveries are
			// deduped by ingestLocked. Spawned under rmu, waited on after.
			wg.Add(1)
			go func(id, receipt string) {
				defer wg.Done()
				_, _ = t.rxQueue.DeleteMessage(ctx, id, receipt, nil)
			}(*msg.MessageID, *msg.PopReceipt)

			if overflow {
				break
			}
		}
		t.rmu.Unlock()

		wg.Wait()
	}

	t.rmu.Lock()
	defer t.rmu.Unlock()

	// Terminal: pending is waiting on a sequence that is not coming, so retries
	// would just ingest one more message per call forever.
	if overflow {
		t.rxErr = ErrReassemblyOverflow
		return nil, t.rxErr
	}

	combined := t.drainLocked()

	// Stall clock: no progress while messages are buffered means a missing
	// sequence. Fail, and stick, for the same reason overflow does.
	switch {
	case len(combined) > 0 || len(t.pending) == 0:
		t.stallSince = time.Time{} // progress, or nothing buffered
	case t.stallSince.IsZero():
		t.stallSince = time.Now() // first blocked poll: start the clock
	case time.Since(t.stallSince) > t.stallLimit():
		t.rxErr = fmt.Errorf("%w: seq %d missing for %s (%d messages buffered)",
			ErrReassemblyStalled, t.rxSeq, t.stallLimit(), len(t.pending))
		return nil, t.rxErr
	}

	if len(combined) == 0 {
		if derr != nil {
			// Reporting this as ErrNoData would make a persistent failure
			// (expired SAS, auth, network) look exactly like an idle queue.
			return nil, derr
		}
		return nil, ErrNoData // out-of-order messages await their predecessors
	}
	return io.NopCloser(bytes.NewReader(combined)), nil
}

func (t *queueTransport) Close() error { return nil }

// MaxRawSize reserves room for the sequence header and a safety margin so the
// base64-encoded message stays under the 64 KiB queue ceiling.
func (t *queueTransport) MaxRawSize() int {
	return (MaxQueueTextMessageSize*3)/4 - seqHeaderSize - queueSizeMargin
}
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
