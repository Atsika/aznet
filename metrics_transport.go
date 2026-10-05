package aznet

import (
	"context"
	"io"
)

type metricsTransport struct {
	Transport
	rot Rotator // nil if underlying transport doesn't support rotation
	m   Metrics
}

type metricsLimitedTransport struct {
	*metricsTransport
	reader limitedRawReader
}

func newMetricsTransport(t Transport, m Metrics) Transport {
	mt := &metricsTransport{Transport: t, m: m}
	if r, ok := t.(Rotator); ok {
		mt.rot = r
	}
	if reader, ok := t.(limitedRawReader); ok {
		return &metricsLimitedTransport{metricsTransport: mt, reader: reader}
	}
	return mt
}

func (t *metricsTransport) WriteRaw(ctx context.Context, seq uint64, data io.ReadSeeker) error {
	var size int64
	if data != nil {
		pos, _ := data.Seek(0, io.SeekCurrent)
		end, _ := data.Seek(0, io.SeekEnd)
		_, _ = data.Seek(pos, io.SeekStart)
		size = end - pos
	}
	err := t.Transport.WriteRaw(ctx, seq, data)
	if err == nil {
		t.m.IncrementWriteTransaction()
		t.m.IncrementBytesSent(size)
	}
	return err
}

func (t *metricsTransport) ReadRaw(ctx context.Context) (io.ReadCloser, error) {
	rc, err := t.Transport.ReadRaw(ctx)
	return t.recordRead(rc, err)
}

func (t *metricsLimitedTransport) ReadRawLimit(ctx context.Context, limit int) (io.ReadCloser, error) {
	rc, err := t.reader.ReadRawLimit(ctx, limit)
	return t.recordRead(rc, err)
}

func (t *metricsTransport) recordRead(rc io.ReadCloser, err error) (io.ReadCloser, error) {
	if err == nil {
		t.m.IncrementReadTransaction()
		return &metricsReadCloser{ReadCloser: rc, m: t.m}, nil
	}
	return nil, err
}

func (t *metricsTransport) ShouldRotate() bool {
	if t.rot != nil {
		return t.rot.ShouldRotate()
	}
	return false
}

func (t *metricsTransport) RotateTX(ctx context.Context) error {
	if t.rot != nil {
		return t.rot.RotateTX(ctx)
	}
	return nil
}

func (t *metricsTransport) RotateRX() error {
	if t.rot != nil {
		return t.rot.RotateRX()
	}
	return nil
}

type metricsReadCloser struct {
	io.ReadCloser
	m Metrics
}

func (r *metricsReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.m.IncrementBytesReceived(int64(n))
	}
	return n, err
}
