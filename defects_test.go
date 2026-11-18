package aznet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestTableBootstrapRowWithNonStringData(t *testing.T) {
	for _, row := range []string{`{"RowKey":"x","Data":123}`, `{"Data":"!!not base64!!"}`, `{"Data":"aGk=","Data01":null}`} {
		if got := extractTableData([]byte(row)); got != nil {
			t.Fatalf("%s: malformed row yielded %q", row, got)
		}
	}
	if got := extractTableData([]byte(`{"Data":"aGk="}`)); string(got) != "hi" {
		t.Fatalf("valid row: %q", got)
	}
}

func TestBlobReadDistinguishesDeletedSession(t *testing.T) {
	code := "BlobNotFound"
	tr := newReadTestBlob(t, func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Header:     http.Header{"Content-Type": []string{"application/xml"}, "X-Ms-Error-Code": []string{code}},
			Body:       io.NopCloser(strings.NewReader("<Error><Code>" + code + "</Code></Error>")),
			Request:    r,
		}, nil
	})
	if _, err := tr.ReadRaw(context.Background()); !errors.Is(err, ErrNoData) {
		t.Fatalf("missing blob must be an empty poll: %v", err)
	}
	code = "ContainerNotFound"
	if _, err := tr.ReadRaw(context.Background()); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("missing container must be terminal: %v", err)
	}
}

// sealedFrames seals each frame type as its own chunk.
func sealedFrames(t *testing.T, n *Noise, types ...byte) [][]byte {
	t.Helper()
	var chunks [][]byte
	for _, typ := range types {
		var frame bytes.Buffer
		f := Frame{Type: typ}
		if typ == MsgTypeData {
			f.Payload = []byte("data")
		}
		BuildFrame(&frame, f)
		raw, err := n.SealData(nil, frame.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, raw)
	}
	return chunks
}

// queuedTransport serves one chunk per ReadRaw, then empty polls.
func queuedTransport(chunks [][]byte) *reviewTransport {
	var mu sync.Mutex
	return &reviewTransport{read: func(context.Context) (io.ReadCloser, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(chunks) == 0 {
			return nil, ErrNoData
		}
		next := chunks[0]
		chunks = chunks[1:]
		return io.NopCloser(bytes.NewReader(next)), nil
	}}
}

func TestProbeSeesPeerWithoutConsumingData(t *testing.T) {
	a, b := reviewNoise(t)
	c := reviewConn(queuedTransport(sealedFrames(t, a, MsgTypePing, MsgTypeData)), b)
	defer c.Close()
	c.peerLastSeen.Store(0)
	if !c.probePeer(context.Background()) || c.peerLastSeen.Load() == 0 {
		t.Fatal("probe missed a buffered ping")
	}
	if !c.probePeer(context.Background()) {
		t.Fatal("probe missed a data chunk")
	}
	if c.probePeer(context.Background()) {
		t.Fatal("probe reported a silent peer as seen")
	}
	// Frames the probe decrypted are still delivered, in order.
	got := make([]byte, 16)
	n, err := c.Read(got)
	if err != nil || string(got[:n]) != "data" {
		t.Fatalf("read after probe: %q %v", got[:n], err)
	}
}

func TestProbeStopsAtReceiveAllowance(t *testing.T) {
	a, b := reviewNoise(t)
	c := reviewConn(queuedTransport(sealedFrames(t, a, MsgTypeData, MsgTypeData, MsgTypeData)), b)
	defer c.Close()
	limits := c.cfg.limits()
	limits.Decrypted = 2 * (FrameHeaderSize + len("data"))
	c.cfg.bufferLimits = limits
	for range 4 {
		c.probePeer(context.Background())
	}
	if c.limitFailure.Load() != nil {
		t.Fatal("an unread backlog was treated as a protocol violation")
	}
	if c.bufs.Read.Len() > limits.Decrypted {
		t.Fatalf("probe buffered %d bytes past the allowance", c.bufs.Read.Len())
	}
}

func TestJanitorKeepsWriteOnlyConnection(t *testing.T) {
	client, server := reviewNoise(t)
	var alive atomic.Bool
	alive.Store(true)
	var mu sync.Mutex
	d := &sessionDriver{}
	d.transport = func(context.Context) (Transport, error) {
		return &reviewTransport{read: func(context.Context) (io.ReadCloser, error) {
			if !alive.Load() {
				return nil, ErrNoData
			}
			mu.Lock()
			defer mu.Unlock()
			var frame bytes.Buffer
			BuildFrame(&frame, Frame{Type: MsgTypePing})
			raw, err := client.SealData(nil, frame.Bytes())
			if err != nil {
				return nil, err
			}
			return io.NopCloser(bytes.NewReader(raw)), nil
		}}, nil
	}
	l := sessionListener(t, d, 1)
	l.cfg.idleTimeout = 100 * time.Millisecond
	accepted, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	c := accepted.(*Conn)
	c.noise = server // pair the accepted connection with the pinging peer
	go l.janitor()
	// The application never reads, yet the peer keeps pinging.
	time.Sleep(10 * l.cfg.idleTimeout)
	if c.closed.Load() != 0 {
		t.Fatal("janitor reaped a connection whose peer was alive")
	}
	alive.Store(false)
	deadline := time.Now().Add(5 * time.Second)
	for c.closed.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("janitor kept a connection whose peer went silent")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFailedChunkIsResentWithoutAnotherWrite(t *testing.T) {
	var calls atomic.Int32
	var first, second []byte
	delivered := make(chan struct{})
	tr := &reviewTransport{write: func(_ context.Context, seq uint64, r io.ReadSeeker) error {
		raw, _ := io.ReadAll(r)
		switch calls.Add(1) {
		case 1:
			first = raw
			return errors.New("injected")
		case 2:
			if seq != 0 {
				t.Errorf("resend changed sequence: %d", seq)
			}
			second = raw
			close(delivered)
		}
		return nil
	}}
	a, _ := reviewNoise(t)
	cfg := applyConfig([]Option{WithPing(0), WithDataPoll(5 * time.Millisecond)})
	ctx, cancel := context.WithCancel(cfg.ctx)
	c := newConn(ctx, cancel, tr, cfg, a, nil, "retry")
	defer c.Close()
	if _, err := c.Write([]byte("once")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-delivered:
	case <-time.After(5 * time.Second):
		t.Fatal("failed chunk waited for another write")
	}
	if !bytes.Equal(first, second) {
		t.Fatal("resend was resealed")
	}
}

func TestDialedIdleTimeoutIsOptIn(t *testing.T) {
	_, b := reviewNoise(t)
	c := reviewConn(&reviewTransport{}, b)
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := c.Read(make([]byte, 1)); errors.Is(err, ErrIdleTimeout) {
		t.Fatal("idle timeout applied without WithIdleTimeout")
	}
	c.SetReadDeadline(time.Time{})
	c.idleLimit = 20 * time.Millisecond
	c.peerLastSeen.Store(0) // long silent, but only this Read's own wait counts
	start := time.Now()
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, ErrIdleTimeout) {
		t.Fatal(err)
	}
	if time.Since(start) < c.idleLimit {
		t.Fatal("a Read was failed for silence that preceded it")
	}
}

func TestSovereignCloudEndpoints(t *testing.T) {
	for host, azure := range map[string]bool{
		"acct.blob.core.windows.net":        true,
		"acct.queue.core.chinacloudapi.cn":  true,
		"acct.table.core.usgovcloudapi.net": true,
		"acct.blob.core.cloudapi.de:443":    true,
		"127.0.0.1:10000":                   false,
		"core.windows.net.example.com":      false,
	} {
		u, _ := url.Parse("https://" + host + "/devstoreaccount1")
		ep := NewEndpoint(u)
		if ep.IsAzure != azure {
			t.Errorf("%s: IsAzure = %v", host, ep.IsAzure)
		}
		if azure && ep.Account != "acct" {
			t.Errorf("%s: account %q", host, ep.Account)
		}
	}
}

func TestSessionAddressesFollowDirection(t *testing.T) {
	cfg := applyConfig(nil)
	u, _ := url.Parse("http://127.0.0.1:10001/devstoreaccount1")
	ep := NewEndpoint(u)
	ep.Key = "a2V5" // any base64 value; no request is sent
	id := "11111111-2222-3333-4444-555555555555"
	for name, factory := range map[string]Factory{queueDriverName: &queueFactory{}, tableDriverName: &tableFactory{}} {
		t.Run(name, func(t *testing.T) {
			var d Driver
			switch name {
			case queueDriverName:
				client, err := newQueueClient(ep, cfg)
				if err != nil {
					t.Fatal(err)
				}
				d = &queueDriver{ep: ep, client: client, cfg: cfg}
			default:
				client, err := newTableClient(ep, cfg)
				if err != nil {
					t.Fatal(err)
				}
				d = &tableDriver{ep: ep, client: client, cfg: cfg}
			}
			_ = factory
			dialed, err := d.NewTransport(context.Background(), id, SessionTokens{Req: "sv=1", Res: "sv=1"}, true)
			if err != nil {
				t.Fatal(err)
			}
			accepted, err := d.NewTransport(context.Background(), id, SessionTokens{}, false)
			if err != nil {
				t.Fatal(err)
			}
			if dialed.LocalAddr().String() != accepted.RemoteAddr().String() || dialed.RemoteAddr().String() != accepted.LocalAddr().String() {
				t.Fatalf("addresses disagree: dialed %s -> %s, accepted %s -> %s", dialed.LocalAddr(), dialed.RemoteAddr(), accepted.LocalAddr(), accepted.RemoteAddr())
			}
			if dialed.LocalAddr().String() == dialed.RemoteAddr().String() {
				t.Fatal("local and remote address are the same resource")
			}
		})
	}
}

func TestSASProtocolFollowsEndpointScheme(t *testing.T) {
	cfg := applyConfig(nil)
	for scheme, want := range map[string]string{"https": "https", "http": "https,http"} {
		u, _ := url.Parse(scheme + "://127.0.0.1:10000/devstoreaccount1")
		ep := NewEndpoint(u)
		ep.Key = "a2V5"
		tokens := map[string]func() (string, string, error){
			blobDriverName:  (&blobDriver{ep: ep, cfg: cfg}).CreateBootstrapTokens,
			queueDriverName: (&queueDriver{ep: ep, cfg: cfg}).CreateBootstrapTokens,
			tableDriverName: (&tableDriver{ep: ep, cfg: cfg}).CreateBootstrapTokens,
		}
		for name, issue := range tokens {
			h, tok, err := issue()
			if err != nil {
				t.Fatalf("%s %s: %v", scheme, name, err)
			}
			for _, sas := range []string{h, tok} {
				values, _ := url.ParseQuery(sas)
				if got := values.Get("spr"); got != want {
					t.Errorf("%s %s: spr = %q, want %q", scheme, name, got, want)
				}
			}
		}
	}
}

func TestListenerCleanupErrorsAreBounded(t *testing.T) {
	l := &Listener{}
	first := errors.New("first failure")
	l.recordCleanup(first)
	for i := range 10 * maxCleanupErrors {
		l.recordCleanup(fmt.Errorf("failure %d", i))
	}
	err := l.cleanupResult()
	if !errors.Is(err, first) {
		t.Fatal("earliest failure lost")
	}
	if lines := strings.Count(err.Error(), "\n") + 1; lines != maxCleanupErrors+1 {
		t.Fatalf("retained %d lines", lines)
	}
	if !strings.Contains(err.Error(), "further cleanup failures omitted") {
		t.Fatal("omitted failures not reported")
	}
}

func TestLaterProxyOptionReplacesInvalidOne(t *testing.T) {
	cfg := applyConfig([]Option{
		WithProxy("socks5://bad", ProxyAuto, "", ""),
		WithProxy("http://proxy.example:8080", ProxyAuto, "", ""),
	})
	defer cfg.cancel()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("stale proxy error: %v", err)
	}
	cfg = applyConfig([]Option{WithProxy("socks5://bad", ProxyAuto, "", "")})
	defer cfg.cancel()
	if err := cfg.Validate(); !errors.Is(err, ErrInvalidProxy) {
		t.Fatalf("invalid proxy accepted: %v", err)
	}
}

// TestAzuriteDeletedSessionIsTerminal confirms against the emulator that a
// dialed transport stops polling once the listener has deleted the session.
func TestAzuriteDeletedSessionIsTerminal(t *testing.T) {
	if os.Getenv("AZNET_AZURITE") != "1" {
		t.Skip("set AZNET_AZURITE=1 with Azurite on localhost:10000-10002")
	}
	const key = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
	for i, scheme := range []string{"azblob", "azqueue", "aztable"} {
		t.Run(scheme, func(t *testing.T) {
			cfg := applyConfig([]Option{WithPing(0)})
			defer cfg.cancel()
			suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
			cfg.handshakeEndpoint, cfg.tokenEndpoint = "h"+suffix, "t"+suffix
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			u := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", 10000+i), Path: "/devstoreaccount1", User: url.UserPassword("devstoreaccount1", key)}
			factory, _ := lookupFactory(scheme)
			driver, err := factory.NewDriver(NewEndpoint(u), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer driver.CleanupBootstrap(ctx)
			id := uuid.NewString()
			tokens, err := driver.CreateSession(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := driver.NewTransport(ctx, id, tokens, false); err != nil {
				t.Fatal(err)
			}
			client, err := driver.NewTransport(ctx, id, tokens, true)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.ReadRaw(ctx); !errors.Is(err, ErrNoData) {
				t.Fatalf("live session: %v", err)
			}
			if err := driver.CleanupSession(ctx, id); err != nil {
				t.Fatal(err)
			}
			if _, err := client.ReadRaw(ctx); !errors.Is(err, ErrSessionGone) {
				t.Fatalf("deleted session: %v", err)
			}
		})
	}
}
