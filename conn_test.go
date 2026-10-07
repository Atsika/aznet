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

func reviewNoise(t *testing.T) (*Noise, *Noise) {
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
			block := func(ctx context.Context) error {
				close(entered)
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
			go func() {
				_, err := c.Write([]byte("x"))
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
