package aznet

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"
)

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
