package aznet

import (
	"context"
	"net"
	"sync/atomic"
)

// Metrics is an interface for tracking connection statistics.
// Drivers call Increment* and collectors read via Get*.
type Metrics interface {
	IncrementWriteTransaction()
	IncrementReadTransaction()
	IncrementListTransaction()
	IncrementDeleteTransaction()
	IncrementBytesSent(n int64)
	IncrementBytesReceived(n int64)

	GetWriteTransactionCount() int64
	GetReadTransactionCount() int64
	GetListTransactionCount() int64
	GetDeleteTransactionCount() int64
	GetBytesSent() int64
	GetBytesReceived() int64
}

// DefaultMetrics implements the Metrics interface with atomic counters.
type DefaultMetrics struct {
	writeTransactions  int64
	readTransactions   int64
	listTransactions   int64
	deleteTransactions int64
	bytesSent          int64
	bytesReceived      int64
}

// NewDefaultMetrics creates a new DefaultMetrics instance.
func NewDefaultMetrics() *DefaultMetrics { return &DefaultMetrics{} }

func (m *DefaultMetrics) IncrementWriteTransaction()     { atomic.AddInt64(&m.writeTransactions, 1) }
func (m *DefaultMetrics) IncrementReadTransaction()      { atomic.AddInt64(&m.readTransactions, 1) }
func (m *DefaultMetrics) IncrementListTransaction()      { atomic.AddInt64(&m.listTransactions, 1) }
func (m *DefaultMetrics) IncrementDeleteTransaction()    { atomic.AddInt64(&m.deleteTransactions, 1) }
func (m *DefaultMetrics) IncrementBytesSent(n int64)     { atomic.AddInt64(&m.bytesSent, n) }
func (m *DefaultMetrics) IncrementBytesReceived(n int64) { atomic.AddInt64(&m.bytesReceived, n) }

func (m *DefaultMetrics) GetWriteTransactionCount() int64 {
	return atomic.LoadInt64(&m.writeTransactions)
}
func (m *DefaultMetrics) GetReadTransactionCount() int64 {
	return atomic.LoadInt64(&m.readTransactions)
}
func (m *DefaultMetrics) GetListTransactionCount() int64 {
	return atomic.LoadInt64(&m.listTransactions)
}
func (m *DefaultMetrics) GetDeleteTransactionCount() int64 {
	return atomic.LoadInt64(&m.deleteTransactions)
}
func (m *DefaultMetrics) GetBytesSent() int64     { return atomic.LoadInt64(&m.bytesSent) }
func (m *DefaultMetrics) GetBytesReceived() int64 { return atomic.LoadInt64(&m.bytesReceived) }

// GetMetrics returns the metrics from a connection if it supports metrics tracking.
// It returns nil if the connection doesn't support metrics.
func GetMetrics(c net.Conn) Metrics {
	type metricsProvider interface{ GetMetrics() Metrics }
	if mp, ok := c.(metricsProvider); ok {
		return mp.GetMetrics()
	}
	return nil
}

type metricsDriver struct {
	Driver
	m Metrics
}

func (d *metricsDriver) PostHandshake(ctx context.Context, connID string, data []byte) error {
	err := d.Driver.PostHandshake(ctx, connID, data)
	if err == nil {
		d.m.IncrementWriteTransaction()
		d.m.IncrementBytesSent(int64(len(data)))
	}
	return err
}

func (d *metricsDriver) GetHandshakes(ctx context.Context) ([]Handshake, error) {
	h, err := d.Driver.GetHandshakes(ctx)
	if err == nil {
		d.m.IncrementReadTransaction()
		d.m.IncrementListTransaction()
	}
	return h, err
}

func (d *metricsDriver) DeleteHandshake(ctx context.Context, id string) error {
	err := d.Driver.DeleteHandshake(ctx, id)
	if err == nil {
		d.m.IncrementDeleteTransaction()
	}
	return err
}

func (d *metricsDriver) PostToken(ctx context.Context, connID string, data []byte) error {
	err := d.Driver.PostToken(ctx, connID, data)
	if err == nil {
		d.m.IncrementWriteTransaction()
		d.m.IncrementBytesSent(int64(len(data)))
	}
	return err
}

func (d *metricsDriver) GetToken(ctx context.Context, connID string) ([]byte, error) {
	data, err := d.Driver.GetToken(ctx, connID)
	if err == nil {
		d.m.IncrementReadTransaction()
		d.m.IncrementBytesReceived(int64(len(data)))
	}
	return data, err
}

func (d *metricsDriver) DeleteToken(ctx context.Context, connID string) error {
	err := d.Driver.DeleteToken(ctx, connID)
	if err == nil {
		d.m.IncrementDeleteTransaction()
	}
	return err
}

func (d *metricsDriver) CreateSession(ctx context.Context, connID string) (SessionTokens, error) {
	t, err := d.Driver.CreateSession(ctx, connID)
	if err == nil {
		d.m.IncrementWriteTransaction()
	}
	return t, err
}

func (d *metricsDriver) NewTransport(ctx context.Context, connID string, tokens SessionTokens, isInitiator bool) (Transport, error) {
	t, err := d.Driver.NewTransport(ctx, connID, tokens, isInitiator)
	if err != nil {
		return nil, err
	}
	return newMetricsTransport(t, d.m), nil
}

func (d *metricsDriver) CleanupBootstrap(ctx context.Context) error {
	err := d.Driver.CleanupBootstrap(ctx)
	if err == nil {
		d.m.IncrementDeleteTransaction()
		d.m.IncrementDeleteTransaction()
	}
	return err
}

func (d *metricsDriver) CleanupSession(ctx context.Context, connID string) error {
	err := d.Driver.CleanupSession(ctx, connID)
	if err == nil {
		d.m.IncrementDeleteTransaction()
	}
	return err
}
