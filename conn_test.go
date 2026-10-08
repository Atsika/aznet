package aznet

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestAzuriteConnectionContract exercises the same encrypted connection contract
// through every registered storage adapter. Only the local emulator is used.
func TestAzuriteConnectionContract(t *testing.T) {
	if os.Getenv("AZNET_AZURITE") != "1" {
		t.Skip("set AZNET_AZURITE=1 with Azurite on localhost:10000-10002")
	}
	// Azurite's public development credential, also used by the examples.
	const key = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
	for i, scheme := range []string{"azblob", "azqueue", "aztable"} {
		t.Run(scheme, func(t *testing.T) {
			cfg := applyConfig([]Option{WithPing(0)})
			defer cfg.cancel()
			suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
			cfg.handshakeEndpoint = "h" + suffix
			cfg.tokenEndpoint = "t" + suffix
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			u := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", 10000+i), Path: "/devstoreaccount1", User: url.UserPassword("devstoreaccount1", key)}
			factory, ok := lookupFactory(scheme)
			if !ok {
				t.Fatal("driver not registered")
			}
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
			defer driver.CleanupSession(ctx, id)
			server, err := driver.NewTransport(ctx, id, tokens, false)
			if err != nil {
				t.Fatal(err)
			}
			client, err := driver.NewTransport(ctx, id, tokens, true)
			if err != nil {
				server.Close()
				t.Fatal(err)
			}
			a, b := reviewNoise(t)
			ac, ax := context.WithCancel(ctx)
			bc, bx := context.WithCancel(ctx)
			sender := newConn(ac, ax, client, cfg, a, nil, id)
			receiver := newConn(bc, bx, server, cfg, b, nil, id)
			defer sender.Close()
			defer receiver.Close()
			sender.SetDeadline(time.Now().Add(20 * time.Second))
			receiver.SetDeadline(time.Now().Add(20 * time.Second))
			payload := bytes.Repeat([]byte("aznet"), sender.MTU()/5+17)
			if n, err := sender.Write(payload); n != len(payload) || err != nil {
				t.Fatal(n, err)
			}
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(receiver, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(payload, got) {
				t.Fatal("forward payload differs")
			}
			// Force a real Blob resource rollover at a frame boundary without 50k writes.
			if blob, ok := client.(*blobTransport); ok {
				blob.txMu.Lock()
				blob.blocksWritten = MaxBlocksPerBlob - 10
				blob.txMu.Unlock()
			}
			if _, err := sender.Write([]byte("tail")); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(receiver, got[:4]); err != nil || string(got[:4]) != "tail" {
				t.Fatal("rollover/tail", err)
			}
			if _, err := receiver.Write([]byte("reply")); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(sender, got[:5]); err != nil || string(got[:5]) != "reply" {
				t.Fatal("reverse payload", err)
			}
			if err := sender.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			if _, err := receiver.Read(got[:1]); err != io.EOF {
				t.Fatalf("FIN: %v", err)
			}
		})
	}
}

type reviewTransport struct {
	write func(context.Context, uint64, io.ReadSeeker) error
	read  func(context.Context) (io.ReadCloser, error)
}

func (t *reviewTransport) WriteRaw(c context.Context, s uint64, r io.ReadSeeker) error {
	if t.write != nil {
		return t.write(c, s, r)
	}
	return nil
}

func (t *reviewTransport) ReadRaw(c context.Context) (io.ReadCloser, error) {
	if t.read != nil {
		return t.read(c)
	}
	return nil, ErrNoData
}

func (t *reviewTransport) Close() error { return nil }

func (t *reviewTransport) LocalAddr() net.Addr { return ServiceAddr{} }

func (t *reviewTransport) RemoteAddr() net.Addr { return ServiceAddr{} }

func (t *reviewTransport) MaxRawSize() int { return 1024 }

func reviewNoise(t testing.TB) (*Noise, *Noise) {
	t.Helper()
	a, _ := NewNoiseClient()
	b, _ := NewNoiseServer()
	m, e := a.WriteMessage(nil)
	if e != nil {
		t.Fatal(e)
	}
	_, e = b.ReadMessage(m)
	if e != nil {
		t.Fatal(e)
	}
	m, e = b.WriteMessage(nil)
	if e != nil {
		t.Fatal(e)
	}
	_, e = a.ReadMessage(m)
	if e != nil {
		t.Fatal(e)
	}
	return a, b
}

func reviewConn(tr Transport, n *Noise) *Conn {
	cfg := applyConfig([]Option{WithPing(0)})
	ctx, cancel := context.WithCancel(cfg.ctx)
	return newConn(ctx, cancel, tr, cfg, n, nil, "review")
}

func TestUpdatedDeadlineInterruptsIO(t *testing.T) {
	for _, read := range []bool{false, true} {
		t.Run(map[bool]string{false: "write", true: "read"}[read], func(t *testing.T) {
			a, _ := reviewNoise(t)
			entered := make(chan struct{})
			var once sync.Once // the barrier may send first, then flushLoop retries
			block := func(ctx context.Context) error {
				once.Do(func() { close(entered) })
				<-ctx.Done()
				return ctx.Err()
			}
			tr := &reviewTransport{write: func(ctx context.Context, _ uint64, _ io.ReadSeeker) error { return block(ctx) }, read: func(ctx context.Context) (io.ReadCloser, error) { return nil, block(ctx) }}
			c := reviewConn(tr, a)
			defer c.cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				if read {
					_, err = c.Read(make([]byte, 1))
				} else if _, err = c.Write([]byte("x")); err == nil {
					_, err = c.Write(nil) // waits behind the blocked send
				}
				done <- err
			}()
			<-entered
			c.SetDeadline(time.Now().Add(10 * time.Millisecond))
			select {
			case err := <-done:
				if !errors.Is(err, os.ErrDeadlineExceeded) {
					t.Fatal(err)
				}
			case <-time.After(200 * time.Millisecond):
				t.Error("updated deadline did not interrupt I/O")
				c.cancel()
				<-done
			}
		})
	}
}

func TestCloseInterruptsWrite(t *testing.T) {
	a, _ := reviewNoise(t)
	entered := make(chan struct{})
	var once sync.Once
	tr := &reviewTransport{write: func(ctx context.Context, _ uint64, _ io.ReadSeeker) error {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return ctx.Err()
	}}
	c := reviewConn(tr, a)
	wd := make(chan struct{})
	go func() {
		c.Write([]byte("x"))
		close(wd)
	}()
	<-entered
	done := make(chan struct{})
	go func() {
		c.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("Close waits indefinitely for flush")
		c.cancel()
		<-done
	}
	<-wd
}

func TestDeadlineExtensionAndClear(t *testing.T) {
	for _, clear := range []bool{false, true} {
		t.Run(fmt.Sprint(clear), func(t *testing.T) {
			a, _ := reviewNoise(t)
			entered := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			tr := &reviewTransport{write: func(ctx context.Context, _ uint64, _ io.ReadSeeker) error {
				once.Do(func() { close(entered) })
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-release:
					return nil
				}
			}}
			c := reviewConn(tr, a)
			defer c.cancel()
			c.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
			done := make(chan error, 1)
			go func() {
				_, err := c.Write([]byte("x"))
				if err == nil {
					_, err = c.Write(nil) // waits behind the blocked send
				}
				done <- err
			}()
			<-entered
			if clear {
				c.SetWriteDeadline(time.Time{})
			} else {
				c.SetWriteDeadline(time.Now().Add(time.Second))
			}
			select {
			case err := <-done:
				t.Fatalf("operation ended after deadline update: %v", err)
			case <-time.After(150 * time.Millisecond):
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCloseBoundWithUncooperativeBackend(t *testing.T) {
	a, _ := reviewNoise(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	tr := &reviewTransport{write: func(context.Context, uint64, io.ReadSeeker) error {
		once.Do(func() { close(entered) })
		<-release
		return io.ErrUnexpectedEOF
	}}
	c := reviewConn(tr, a)
	done := make(chan struct{})
	go func() {
		c.Write([]byte("x"))
		close(done)
	}()
	<-entered
	start := time.Now()
	err := c.Close()
	if !errors.Is(err, os.ErrDeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatal(err, time.Since(start))
	}
	close(release)
	<-done
}

func TestGracefulCloseDeliversFin(t *testing.T) {
	a, b := reviewNoise(t)
	var types []byte
	tr := &reviewTransport{write: func(_ context.Context, _ uint64, r io.ReadSeeker) error {
		raw, _ := io.ReadAll(r)
		plain, _, err := b.UnsealData(nil, raw, 1024)
		if err != nil {
			return err
		}
		for len(plain) > 0 {
			types = append(types, plain[4])
			plain = plain[5+int(binary.BigEndian.Uint32(plain)):]
		}
		return nil
	}}
	c := reviewConn(tr, a)
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(types, []byte{MsgTypeData, MsgTypeFin}) {
		t.Fatal(types)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdatedDeadlineInterruptsPollAndWaiters(t *testing.T) {
	a, _ := reviewNoise(t)
	entered := make(chan struct{})
	var once sync.Once
	c := reviewConn(&reviewTransport{read: func(context.Context) (io.ReadCloser, error) {
		once.Do(func() { close(entered) })
		return nil, ErrNoData
	}}, a)
	defer c.cancel()
	c.poll = NewAdaptivePoll(time.Hour, time.Hour)
	done := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := c.Read(make([]byte, 1))
			done <- err
		}()
	}
	<-entered
	c.SetReadDeadline(time.Now().Add(-time.Second))
	for range 2 {
		select {
		case err := <-done:
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("read poll or waiter not interrupted")
		}
	}
	c.SetReadDeadline(time.Time{})
	c.SetReadDeadline(time.Now().Add(time.Millisecond))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal(err)
	}
}

type blockingCloseTransport struct {
	reviewTransport
	release  chan struct{}
	finished chan struct{}
}

func (tr *blockingCloseTransport) Close() error {
	<-tr.release
	close(tr.finished)
	return nil
}

func TestCloseBoundsTransportClose(t *testing.T) {
	a, _ := reviewNoise(t)
	tr := &blockingCloseTransport{release: make(chan struct{}), finished: make(chan struct{})}
	c := reviewConn(tr, a)
	start := time.Now()
	err := c.Close()
	if !errors.Is(err, os.ErrDeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatal(err, time.Since(start))
	}
	close(tr.release)
	<-tr.finished
}

// Prime the real receive path, leaving authenticated plaintext in the buffer.
// Small subsequent reads must not create an I/O cancellation context per byte.
func TestBufferedReadDoesNotAllocate(t *testing.T) {
	a, b := reviewNoise(t)
	var plain bytes.Buffer
	BuildFrame(&plain, Frame{Type: MsgTypeData, Payload: bytes.Repeat([]byte{42}, 900)})
	sealed, err := a.SealData(nil, plain.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	c := reviewConn(&reviewTransport{read: func(context.Context) (io.ReadCloser, error) {
		calls++
		return io.NopCloser(bytes.NewReader(sealed)), nil
	}}, b)
	defer c.Close()
	var p [1]byte
	if _, err := c.Read(p[:]); err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(100, func() {
		if n, err := c.Read(p[:]); n != 1 || err != nil || p[0] != 42 {
			t.Fatalf("buffered read: %d %v %v", n, err, p)
		}
	})
	if allocs != 0 {
		t.Fatalf("buffered read allocated %.0f times per call", allocs)
	}
	if calls != 1 {
		t.Fatalf("buffered reads made %d transport calls", calls)
	}
}

func TestBufferedReadHonorsLifecycle(t *testing.T) {
	for _, condition := range []string{"deadline", "canceled", "closed", "overflow", "gate"} {
		t.Run(condition, func(t *testing.T) {
			a, b := reviewNoise(t)
			tr := &benchmarkTransport{}
			sender, receiver := reviewConn(tr, a), reviewConn(tr, b)
			defer sender.cancel()
			defer receiver.Close()
			if _, err := sender.Write([]byte("abcdef")); err != nil {
				t.Fatal(err)
			}
			if _, err := sender.Write(nil); err != nil {
				t.Fatal(err)
			}
			var p [1]byte
			if _, err := receiver.Read(p[:]); err != nil || p[0] != 'a' {
				t.Fatal(p, err)
			}
			want := error(os.ErrDeadlineExceeded)
			switch condition {
			case "deadline":
				receiver.SetReadDeadline(time.Now().Add(-time.Second))
			case "canceled":
				receiver.cancel()
				want = context.Canceled
			case "closed":
				_ = receiver.Close()
				want = net.ErrClosed
			case "overflow":
				receiver.overflow("test")
				want = ErrBufferLimit
			case "gate":
				receiver.readGate <- struct{}{}
				defer func() { <-receiver.readGate }()
				receiver.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
			}
			n, err := receiver.Read(p[:])
			if n != 0 || !errors.Is(err, want) {
				t.Fatalf("read after %s: %d, %v; want %v", condition, n, err, want)
			}
			if condition == "deadline" {
				receiver.SetReadDeadline(time.Time{})
				if n, err := receiver.Read(p[:]); n != 1 || err != nil || p[0] != 'b' {
					t.Fatalf("deadline consumed buffered bytes: %d %v %v", n, err, p)
				}
			}
		})
	}
}

// Decryption may reuse scratch storage only after the prior plaintext has been
// consumed. Include a frame split across encrypted chunks (legacy/custom peers)
// and small application reads, then verify the following frame and ordered FIN.
func TestReadPlaintextBufferOwnership(t *testing.T) {
	a, b := reviewNoise(t)
	var first, second bytes.Buffer
	left := bytes.Repeat([]byte("first"), 120)
	right := bytes.Repeat([]byte("second"), 100)
	BuildFrame(&first, Frame{Type: MsgTypeData, Payload: left})
	BuildFrame(&second, Frame{Type: MsgTypeData, Payload: right})
	BuildFrame(&second, Frame{Type: MsgTypeFin})
	var sealed []byte
	for _, plain := range [][]byte{first.Bytes()[:300], first.Bytes()[300:], second.Bytes()} {
		raw, err := a.SealData(nil, plain)
		if err != nil {
			t.Fatal(err)
		}
		sealed = append(sealed, raw...)
	}
	calls := 0
	c := reviewConn(&reviewTransport{read: func(context.Context) (io.ReadCloser, error) {
		calls++
		if calls > 1 {
			return nil, errors.New("unexpected extra fetch")
		}
		return io.NopCloser(bytes.NewReader(sealed)), nil
	}}, b)
	defer c.Close()
	want := append(left, right...)
	var got []byte
	for {
		var p [7]byte
		n, err := c.Read(p[:])
		got = append(got, p[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(got, want) {
		t.Fatal("decryption overwrote unread plaintext or lost frame suffix")
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

func TestConcurrentReadPreservesOrder(t *testing.T) {
	a, b := reviewNoise(t)
	var chunks [][]byte
	for _, s := range []string{"a", "b"} {
		var f bytes.Buffer
		BuildFrame(&f, Frame{Type: MsgTypeData, Payload: []byte(s)})
		raw, _ := a.SealData(nil, f.Bytes())
		chunks = append(chunks, raw)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	second := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	tr := &reviewTransport{read: func(ctx context.Context) (io.ReadCloser, error) {
		mu.Lock()
		i := calls
		calls++
		mu.Unlock()
		if i == 0 {
			close(entered)
			<-release
		} else if i == 1 {
			close(second)
		} else {
			return nil, ErrNoData
		}
		return io.NopCloser(bytes.NewReader(chunks[i])), nil
	}}
	c := reviewConn(tr, b)
	defer c.cancel()
	done := make(chan error, 2)
	go func() {
		_, e := c.Read(make([]byte, 1))
		done <- e
	}()
	<-entered
	go func() {
		_, e := c.Read(make([]byte, 1))
		done <- e
	}()
	select {
	case <-second:
		t.Error("concurrent receive overtook first fetch")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}

type blockingBody struct {
	entered, closed chan struct{}
	once            sync.Once
}

func (b *blockingBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *blockingBody) Close() error {
	close(b.closed)
	return nil
}

func TestReadDeadlineInterruptsBody(t *testing.T) {
	a, _ := reviewNoise(t)
	body := &blockingBody{entered: make(chan struct{}), closed: make(chan struct{})}
	c := reviewConn(&reviewTransport{read: func(context.Context) (io.ReadCloser, error) { return body, nil }}, a)
	defer c.cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.Read(make([]byte, 1))
		done <- err
	}()
	<-body.entered
	c.SetReadDeadline(time.Now().Add(-time.Second))
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("body not interrupted")
	}
}

type cancelBody struct {
	*bytes.Reader
	cancel func()
}

func (b *cancelBody) Read(p []byte) (int, error) {
	n, e := b.Reader.Read(p)
	if e == io.EOF {
		b.cancel()
	}
	return n, e
}

func TestReadRecoversBufferedCiphertextAfterDeadline(t *testing.T) {
	sender, receiver := reviewNoise(t)
	var plain bytes.Buffer
	BuildFrame(&plain, Frame{Type: MsgTypeData, Payload: []byte("hello")})
	sealed, e := sender.SealData(nil, plain.Bytes())
	if e != nil {
		t.Fatal(e)
	}
	var c *Conn
	calls := 0
	tr := &reviewTransport{read: func(ctx context.Context) (io.ReadCloser, error) {
		calls++
		if calls == 1 {
			return &cancelBody{bytes.NewReader(sealed), func() { c.SetReadDeadline(time.Now().Add(-time.Second)) }}, nil
		}
		return nil, ErrNoData
	}}
	c = reviewConn(tr, receiver)
	defer c.Close()
	b := make([]byte, 5)
	_, err := c.Read(b)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(40 * time.Millisecond))
	n, err := c.Read(b)
	if n != 5 || string(b) != "hello" || err != nil {
		t.Fatalf("recovery n=%d bytes=%q error=%v retainedCiphertext=%d", n, b, err, c.bufs.Noise.Len())
	}
}

func TestLargeWriteDoesNotQueueEntirePayload(t *testing.T) {
	a, _ := reviewNoise(t)
	var c *Conn
	c = reviewConn(&reviewTransport{write: func(context.Context, uint64, io.ReadSeeker) error {
		if queued(c) > 8<<20 {
			return fmt.Errorf("queued %d bytes, exceeds 8 MiB", queued(c))
		}
		return nil
	}}, a)
	defer c.cancel()
	payload := bytes.Repeat([]byte("x"), 9<<20)
	if n, err := c.Write(payload); err != nil || n != len(payload) {
		t.Fatal(n, err)
	}
}

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
		if len(raw) > 40 || queued(c) > 64 {
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
	if len(c.pending.data) > 40 || queued(c) > 64 {
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
	if _, err := c.Write(nil); err != nil {
		t.Fatal(err)
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
		if queued(c) > 64 || len(c.pending.data) > 40 {
			t.Fatal("failures accumulated unbounded bytes")
		}
	}
	if err := c.CloseWrite(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	if queued(c) > 64 {
		t.Fatal("FIN exceeded allowance")
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
				peak = max(peak, queued(c))
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
			if _, err := c.Write(nil); err != nil {
				t.Fatal(err)
			}
			if received != len(payload) || peak > allowance {
				t.Fatal("byte bounds or accounting")
			}
			t.Logf("payload=%d write_allowance=%d peak_write=%d requests=%d retry_limit=%d", len(payload), allowance, peak, requests, cfg.limits().Retry)
		})
	}
}

func TestWriteAcceptedBytesAndRetry(t *testing.T) {
	a, b := reviewNoise(t)
	var attempts [][]byte
	var seqs []uint64
	tr := &reviewTransport{write: func(_ context.Context, s uint64, r io.ReadSeeker) error {
		raw, _ := io.ReadAll(r)
		attempts = append(attempts, raw)
		seqs = append(seqs, s)
		if len(attempts) == 1 {
			return io.ErrUnexpectedEOF
		}
		return nil
	}}
	c := reviewConn(tr, a)
	defer c.cancel()
	n, err := c.Write([]byte("abc"))
	if n != 3 || err != nil {
		t.Errorf("accepted bytes = %d, %v; want 3", n, err)
	}
	owedFailure(t, c)
	if err := c.flush(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(attempts[0], attempts[1]) || seqs[0] != seqs[1] {
		t.Fatal("retry changed ciphertext or sequence")
	}
	plain, _, err := b.UnsealData(nil, attempts[1], 1024)
	if err != nil || !bytes.Equal(plain, []byte{0, 0, 0, 3, MsgTypeData, 'a', 'b', 'c'}) {
		t.Fatal(plain, err)
	}
}

type rotatingTransport struct {
	reviewTransport
	rotate    bool
	fail      bool
	rotations int
}

func (r *rotatingTransport) ShouldRotate() bool { return r.rotate }

func (r *rotatingTransport) RotateRX() error { return nil }

func (r *rotatingTransport) RotateTX(context.Context) error {
	r.rotations++
	if r.fail {
		r.fail = false
		return io.ErrUnexpectedEOF
	}
	r.rotate = false
	return nil
}

func TestRolloverFailurePreservesPending(t *testing.T) {
	a, b := reviewNoise(t)
	var chunks [][]byte
	tr := &rotatingTransport{rotate: true, fail: true}
	tr.write = func(_ context.Context, _ uint64, r io.ReadSeeker) error {
		raw, _ := io.ReadAll(r)
		chunks = append(chunks, raw)
		return nil
	}
	c := reviewConn(tr, a)
	defer c.cancel()
	n, err := c.Write([]byte("abc"))
	if n != 3 || err != nil {
		t.Fatal(n, err)
	}
	owedFailure(t, c)
	if !c.pending.valid || !c.pending.rotate || len(c.pending.data) == 0 {
		t.Fatal("rotation failure discarded pending ciphertext")
	}
	pending := bytes.Clone(c.pending.data)
	if err := c.flush(); err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 || tr.rotations != 2 {
		t.Fatalf("chunks=%d rotations=%d", len(chunks), tr.rotations)
	}
	if !bytes.Equal(pending, chunks[0]) {
		t.Fatal("rotation ciphertext changed")
	}
	for i, raw := range chunks {
		plain, _, err := b.UnsealData(nil, raw, 1024)
		if err != nil {
			t.Fatal(err)
		}
		want := MsgTypeRotate
		if i == 1 {
			want = MsgTypeData
		}
		if plain[4] != want {
			t.Fatal(plain)
		}
	}
}

func TestFrameAlignedRollover(t *testing.T) {
	a, b := reviewNoise(t)
	var types []byte
	var got []byte
	var seqs []uint64
	tr := &rotatingTransport{}
	tr.write = func(_ context.Context, seq uint64, r io.ReadSeeker) error {
		raw, _ := io.ReadAll(r)
		plain, _, err := b.UnsealData(nil, raw, 1024)
		if err != nil {
			return err
		}
		seqs = append(seqs, seq)
		for len(plain) > 0 {
			if len(plain) < FrameHeaderSize {
				t.Fatal("split header")
			}
			n := int(binary.BigEndian.Uint32(plain))
			if len(plain) < FrameHeaderSize+n {
				t.Fatal("split frame")
			}
			types = append(types, plain[4])
			if plain[4] == MsgTypeData {
				got = append(got, plain[5:5+n]...)
			}
			plain = plain[5+n:]
		}
		if len(seqs) == 1 {
			tr.rotate = true
		}
		return nil
	}
	c := reviewConn(tr, a)
	defer c.cancel()
	payload := bytes.Repeat([]byte("x"), 2*c.MTU()+7)
	if n, err := c.Write(payload); n != len(payload) || err != nil {
		t.Fatal(n, err)
	}
	if _, err := c.Write(nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, got) || !bytes.Equal(types, []byte{MsgTypeData, MsgTypeRotate, MsgTypeData, MsgTypeData}) {
		t.Fatal(types, len(got))
	}
	for i, s := range seqs {
		if s != uint64(i) {
			t.Fatal(seqs)
		}
	}
}

func TestWriteDeadlineInterruptsFlushWaiter(t *testing.T) {
	a, _ := reviewNoise(t)
	entered := make(chan struct{})
	var once sync.Once
	tr := &reviewTransport{write: func(ctx context.Context, _ uint64, _ io.ReadSeeker) error {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return ctx.Err()
	}}
	c := reviewConn(tr, a)
	defer c.cancel()
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	<-entered
	// Both barriers wait behind the blocked send; neither may hang.
	done := make(chan error, 2)
	for range 2 {
		go func() { _, err := c.Write(nil); done <- err }()
	}
	c.SetWriteDeadline(time.Now().Add(-time.Second))
	for range 2 {
		select {
		case err := <-done:
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("write or flush waiter not interrupted")
		}
	}
}

// batchRecorder decrypts every landed chunk, so tests can assert ordering,
// exactly-once delivery and the number of storage writes.
type batchRecorder struct {
	t       *testing.T
	peer    *Noise
	release chan struct{} // when non-nil, each write waits for one token
	fail    int           // fail this many first attempts
	mu      sync.Mutex
	seqs    []uint64
	data    []byte
	writes  int
	raws    [][]byte      // ciphertext of every attempt, in order
	chunks  [][]frameInfo // frame layout of every delivered chunk
}

type frameInfo struct {
	kind byte
	size int
}

func (r *batchRecorder) write(ctx context.Context, seq uint64, rs io.ReadSeeker) error {
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	raw, err := io.ReadAll(rs)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seqs = append(r.seqs, seq)
	r.raws = append(r.raws, raw)
	if r.fail > 0 {
		r.fail--
		return io.ErrUnexpectedEOF
	}
	plain, _, err := r.peer.UnsealData(nil, raw, len(raw))
	if err != nil {
		r.t.Error("unseal:", err)
		return err
	}
	var frames []frameInfo
	for len(plain) > 0 {
		n := int(binary.BigEndian.Uint32(plain))
		frames = append(frames, frameInfo{plain[4], n})
		if plain[4] == MsgTypeData {
			r.data = append(r.data, plain[FrameHeaderSize:FrameHeaderSize+n]...)
		}
		plain = plain[FrameHeaderSize+n:]
	}
	r.chunks = append(r.chunks, frames)
	r.writes++
	return nil
}

// queued reads the write buffer length without racing flushLoop.
func queued(c *Conn) int {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.bufs.Write.Len()
}

// owedFailure waits for flushLoop to record a background send failure.
func owedFailure(t *testing.T, c *Conn) error {
	t.Helper()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if failed := c.flushErr.Load(); failed != nil {
			return *failed
		}
	}
	t.Fatal("no background send failure recorded")
	return nil
}

func batchConn(t *testing.T, r *batchRecorder, opts ...Option) *Conn {
	a, b := reviewNoise(t)
	r.t, r.peer = t, b
	cfg := applyConfig(append([]Option{WithPing(0)}, opts...))
	ctx, cancel := context.WithCancel(cfg.ctx)
	c := newConn(ctx, cancel, &reviewTransport{write: r.write}, cfg, a, nil, "batch")
	t.Cleanup(cancel)
	return c
}

func TestBatchedWritesCoalesceInOrder(t *testing.T) {
	r := &batchRecorder{release: make(chan struct{})}
	c := batchConn(t, r)
	var want []byte
	for i := range 50 {
		p := bytes.Repeat([]byte{byte(i)}, 7)
		want = append(want, p...)
		// Must not wait for storage: the first chunk is still blocked in flight.
		if n, err := c.Write(p); n != len(p) || err != nil {
			t.Fatal(n, err)
		}
	}
	close(r.release)
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !bytes.Equal(r.data, want) {
		t.Fatal("batched payload differs or is out of order")
	}
	// The first write leaves alone, the other 49 share chunks bounded by MTU
	// (1 KiB test transport), plus the FIN; synchronous writes would need 51.
	if r.writes > 4 {
		t.Fatalf("%d storage writes for 50 small Writes", r.writes)
	}
}

func TestBatchedWriteBlocksAtLimitAndHonorsDeadline(t *testing.T) {
	r := &batchRecorder{release: make(chan struct{})}
	c := batchConn(t, r, WithBufferLimits(BufferLimits{Write: 256}))
	c.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
	n, err := c.Write(make([]byte, 4096))
	if !errors.Is(err, os.ErrDeadlineExceeded) || n <= 0 || n >= 4096 {
		t.Fatal(n, err)
	}
	if n := queued(c); n > 256 {
		t.Fatalf("queued %d bytes beyond the 256-byte allowance", n)
	}
	close(r.release)
	c.SetWriteDeadline(time.Time{})
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if len(r.data) != n {
		t.Fatalf("delivered %d of %d accepted bytes", len(r.data), n)
	}
}

func TestBatchedFailureResendsSameChunk(t *testing.T) {
	r := &batchRecorder{fail: 1}
	c := batchConn(t, r)
	if _, err := c.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	// The background failure surfaces on a later Write without consuming it.
	owedFailure(t, c)
	if n, err := c.Write([]byte("second")); n != 0 || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(n, err)
	}
	if _, err := c.Write([]byte("second")); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if string(r.data) != "firstsecond" {
		t.Fatalf("delivered %q", r.data)
	}
	if r.seqs[0] != r.seqs[1] {
		t.Fatal("retry used a new sequence", r.seqs)
	}
}

// Writes queued while a chunk is in flight extend one DATA frame instead of
// each adding a frame, so chunks fill to the MTU; the sealed chunk in flight is
// never modified and bytes stay in order.
func TestSmallWritesExtendOneFrame(t *testing.T) {
	r := &batchRecorder{release: make(chan struct{})}
	c := batchConn(t, r) // 1 KiB transport: MTU just under 1 KiB
	var want []byte
	write := func(b []byte) {
		t.Helper()
		want = append(want, b...)
		if n, err := c.Write(b); n != len(b) || err != nil {
			t.Fatal(n, err)
		}
	}
	write([]byte("first"))
	// Let the first chunk be sealed and held in flight before the next writes.
	deadline := time.Now().Add(time.Second)
	for {
		c.wmu.Lock()
		sealed := c.sealedLen
		c.wmu.Unlock()
		if sealed > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first chunk not sealed")
		}
		time.Sleep(time.Millisecond)
	}
	for i := range 200 {
		write(bytes.Repeat([]byte{byte(i)}, 7))
	}
	close(r.release)
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !bytes.Equal(r.data, want) {
		t.Fatal("extended frames lost or reordered bytes")
	}
	frames := 0
	for _, chunk := range r.chunks {
		for _, f := range chunk {
			if f.kind == MsgTypeData {
				frames++
				if f.size > c.MTU() {
					t.Fatalf("frame of %d bytes exceeds MTU %d", f.size, c.MTU())
				}
			}
		}
	}
	// 1405 bytes need two frames at a ~1 KiB MTU, plus the first: three, not 201.
	if frames > 3 {
		t.Fatalf("%d DATA frames for 201 writes", frames)
	}
}

// A retried chunk resends its exact ciphertext even though writes made after
// it was sealed extended the tail; those bytes follow in later chunks.
func TestExtensionKeepsRetriedChunkIntact(t *testing.T) {
	r := &batchRecorder{fail: 1}
	c := batchConn(t, r)
	if _, err := c.Write([]byte("sealed")); err != nil {
		t.Fatal(err)
	}
	owedFailure(t, c)
	// The owed failure surfaces on the next Write, which accepts nothing.
	if n, err := c.Write([]byte("later")); n != 0 || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(n, err)
	}
	if _, err := c.Write([]byte("later")); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !bytes.Equal(r.raws[0], r.raws[1]) || r.seqs[0] != r.seqs[1] {
		t.Fatal("retry changed the sealed chunk")
	}
	if string(r.data) != "sealedlater" {
		t.Fatalf("delivered %q", r.data)
	}
}

// Control frames end the extendable tail: data never merges across a PING.
func TestExtensionStopsAtControlFrames(t *testing.T) {
	r := &batchRecorder{release: make(chan struct{})}
	c := batchConn(t, r)
	if _, err := c.Write([]byte("in flight")); err != nil {
		t.Fatal(err)
	}
	for {
		c.wmu.Lock()
		sealed := c.sealedLen
		c.wmu.Unlock()
		if sealed > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := c.Write([]byte("before")); err != nil {
		t.Fatal(err)
	}
	c.wmu.Lock()
	c.appendControl(MsgTypePing)
	c.wmu.Unlock()
	if _, err := c.Write([]byte("after")); err != nil {
		t.Fatal(err)
	}
	close(r.release)
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var layout []frameInfo
	for _, chunk := range r.chunks[1:] {
		layout = append(layout, chunk...)
	}
	want := []frameInfo{{MsgTypeData, 6}, {MsgTypePing, 0}, {MsgTypeData, 5}, {MsgTypeFin, 0}}
	if fmt.Sprint(layout) != fmt.Sprint(want) {
		t.Fatalf("frames %v, want %v", layout, want)
	}
}
