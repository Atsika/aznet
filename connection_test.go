package aznet

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

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
func (t *reviewTransport) Close() error         { return nil }
func (t *reviewTransport) LocalAddr() net.Addr  { return ServiceAddr{} }
func (t *reviewTransport) RemoteAddr() net.Addr { return ServiceAddr{} }
func (t *reviewTransport) MaxRawSize() int      { return 1024 }
func reviewNoise(t *testing.T) (*Noise, *Noise) {
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
	if n != 3 || err == nil {
		t.Errorf("accepted bytes = %d, %v; want 3 and error", n, err)
	}
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
func TestUpdatedDeadlineInterruptsIO(t *testing.T) {
	for _, read := range []bool{false, true} {
		t.Run(map[bool]string{false: "write", true: "read"}[read], func(t *testing.T) {
			a, _ := reviewNoise(t)
			entered := make(chan struct{})
			block := func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() }
			tr := &reviewTransport{write: func(ctx context.Context, _ uint64, _ io.ReadSeeker) error { return block(ctx) }, read: func(ctx context.Context) (io.ReadCloser, error) { return nil, block(ctx) }}
			c := reviewConn(tr, a)
			defer c.cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				if read {
					_, err = c.Read(make([]byte, 1))
				} else {
					_, err = c.Write([]byte("x"))
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
	go func() { c.Write([]byte("x")); close(wd) }()
	<-entered
	done := make(chan struct{})
	go func() { c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("Close waits indefinitely for flush")
		c.cancel()
		<-done
	}
	<-wd
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
	go func() { _, e := c.Read(make([]byte, 1)); done <- e }()
	<-entered
	go func() { _, e := c.Read(make([]byte, 1)); done <- e }()
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

type rotatingTransport struct {
	reviewTransport
	rotate    bool
	fail      bool
	rotations int
}

func (r *rotatingTransport) ShouldRotate() bool { return r.rotate }
func (r *rotatingTransport) RotateRX() error    { return nil }
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
	if n != 3 || err == nil {
		t.Fatal(n, err)
	}
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
	if !bytes.Equal(payload, got) || !bytes.Equal(types, []byte{MsgTypeData, MsgTypeRotate, MsgTypeData, MsgTypeData}) {
		t.Fatal(types, len(got))
	}
	for i, s := range seqs {
		if s != uint64(i) {
			t.Fatal(seqs)
		}
	}
}
func TestDeadlineExtensionAndClear(t *testing.T) {
	for _, clear := range []bool{false, true} {
		t.Run(fmt.Sprint(clear), func(t *testing.T) {
			a, _ := reviewNoise(t)
			entered := make(chan struct{})
			release := make(chan struct{})
			tr := &reviewTransport{write: func(ctx context.Context, _ uint64, _ io.ReadSeeker) error {
				close(entered)
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
			go func() { _, err := c.Write([]byte("x")); done <- err }()
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
	go func() { c.Write([]byte("x")); close(done) }()
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

type blockingBody struct {
	entered, closed chan struct{}
	once            sync.Once
}

func (b *blockingBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.closed
	return 0, io.ErrClosedPipe
}
func (b *blockingBody) Close() error { close(b.closed); return nil }
func TestReadDeadlineInterruptsBody(t *testing.T) {
	a, _ := reviewNoise(t)
	body := &blockingBody{entered: make(chan struct{}), closed: make(chan struct{})}
	c := reviewConn(&reviewTransport{read: func(context.Context) (io.ReadCloser, error) { return body, nil }}, a)
	defer c.cancel()
	done := make(chan error, 1)
	go func() { _, err := c.Read(make([]byte, 1)); done <- err }()
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
		go func() { _, err := c.Read(make([]byte, 1)); done <- err }()
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

func (tr *blockingCloseTransport) Close() error { <-tr.release; close(tr.finished); return nil }
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
	done := make(chan error, 2)
	write := func() {
		n, err := c.Write([]byte("x"))
		if n != 1 {
			done <- fmt.Errorf("accepted %d bytes", n)
			return
		}
		done <- err
	}
	go write()
	<-entered
	go write()
	// Wait until the second writer has accepted its frame behind the first.
	limit := time.After(time.Second)
	for {
		c.wmu.Lock()
		queued := c.bufs.Write.Len()
		c.wmu.Unlock()
		if queued == 2*(FrameHeaderSize+1) {
			break
		}
		select {
		case <-limit:
			t.Fatal("second writer did not queue")
		default:
			time.Sleep(time.Millisecond)
		}
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
