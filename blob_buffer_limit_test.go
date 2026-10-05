package aznet

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"testing"
)

func TestBlobRangeUsesRemainingReceiveAllowance(t *testing.T) {
	for _, metrics := range []bool{false, true} {
		t.Run(fmt.Sprint(metrics), func(t *testing.T) {
			a, b := reviewNoise(t)
			var ciphertext []byte
			for range 3 {
				var frame bytes.Buffer
				BuildFrame(&frame, Frame{Type: MsgTypeData, Payload: bytes.Repeat([]byte{42}, 875)})
				raw, err := a.SealData(nil, frame.Bytes())
				if err != nil {
					t.Fatal(err)
				}
				ciphertext = append(ciphertext, raw...)
			}
			var c *Conn
			tr := newReadTestBlob(t, func(r *http.Request) (*http.Response, error) {
				var start, end int
				if _, err := fmt.Sscanf(blobRequestRange(r), "bytes=%d-%d", &start, &end); err != nil {
					return nil, err
				}
				if end-start+1 > 1000-c.bufs.Noise.Len() {
					t.Error("range exceeds remaining allowance")
				}
				end = min(end+1, len(ciphertext))
				return blobReadResponse(r, io.NopCloser(bytes.NewReader(ciphertext[start:end])), end-start), nil
			})
			cfg := applyConfig([]Option{WithPing(0), WithBufferLimits(BufferLimits{Pending: 1000, Retry: 1000})})
			tr.cfg = cfg
			var transport Transport = tr
			if metrics {
				transport = newMetricsTransport(tr, NewDefaultMetrics())
			}
			c = newConn(cfg.ctx, cfg.cancel, transport, cfg, b, nil, "bounded")
			defer cfg.cancel()
			got := make([]byte, 3*875)
			if _, err := io.ReadFull(c, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, bytes.Repeat([]byte{42}, len(got))) {
				t.Fatal("bounded ranges changed byte identity or order")
			}
		})
	}
}
