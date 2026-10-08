package aznet

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"testing"
)

// A bounded, synchronous transport isolates the shared connection's framing,
// encryption, deadlines and buffering from HTTP/storage scheduling. Each block
// is consumed before the next write; this is a CPU benchmark, not a cloud model.
type benchmarkTransport struct{ raw bytes.Buffer }

func (t *benchmarkTransport) WriteRaw(_ context.Context, _ uint64, r io.ReadSeeker) error {
	_, err := io.Copy(&t.raw, r)
	return err
}
func (t *benchmarkTransport) ReadRaw(context.Context) (io.ReadCloser, error) {
	if t.raw.Len() == 0 {
		return nil, ErrNoData
	}
	return io.NopCloser(&t.raw), nil
}
func (t *benchmarkTransport) Close() error         { return nil }
func (t *benchmarkTransport) MaxRawSize() int      { return 64 << 10 }
func (t *benchmarkTransport) LocalAddr() net.Addr  { return ServiceAddr{} }
func (t *benchmarkTransport) RemoteAddr() net.Addr { return ServiceAddr{} }

func BenchmarkConnTransfer(b *testing.B) {
	for _, size := range []int{1, 64, 1024, 32 << 10, 64 << 10} {
		b.Run(fmt.Sprintf("read%d", size), func(b *testing.B) {
			a, z := reviewNoise(b)
			tr := &benchmarkTransport{}
			sender, receiver := reviewConn(tr, a), reviewConn(tr, z)
			defer sender.cancel()
			defer receiver.cancel()
			payload := bytes.Repeat([]byte{42}, 64<<10)
			got := make([]byte, size)
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if n, err := sender.Write(payload); n != len(payload) || err != nil {
					b.Fatal(n, err)
				}
				// The transport is unsynchronized: wait until the payload is sent.
				if _, err := sender.Write(nil); err != nil {
					b.Fatal(err)
				}
				for left := len(payload); left > 0; {
					n, err := receiver.Read(got[:min(len(got), left)])
					if err != nil || n == 0 {
						b.Fatal(n, err)
					}
					left -= n
				}
			}
		})
	}
}
