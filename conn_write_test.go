package aznet

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestLargeWriteDoesNotQueueEntirePayload(t *testing.T) {
	a, _ := reviewNoise(t)
	var c *Conn
	c = reviewConn(&reviewTransport{write: func(context.Context, uint64, io.ReadSeeker) error {
		if c.bufs.Write.Len() > 8<<20 {
			return fmt.Errorf("queued %d bytes, exceeds 8 MiB", c.bufs.Write.Len())
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
		if len(raw) > 40 || c.bufs.Write.Len() > 64 {
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
	if len(c.pending.data) > 40 || c.bufs.Write.Len() > 64 {
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
		if c.bufs.Write.Len() > 64 || len(c.pending.data) > 40 {
			t.Fatal("failures accumulated unbounded bytes")
		}
	}
	if err := c.CloseWrite(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	if c.bufs.Write.Len() > 64 {
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
				peak = max(peak, c.bufs.Write.Len())
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
	write := func(want int) {
		n, err := c.Write([]byte("x"))
		if n != want {
			done <- fmt.Errorf("accepted %d bytes", n)
			return
		}
		done <- err
	}
	go write(1)
	<-entered
	go write(0)
	// The second writer waits for ownership without accepting unbounded data.
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
