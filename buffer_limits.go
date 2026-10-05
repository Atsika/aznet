package aznet

import (
	"errors"
	"fmt"
)

// ErrBufferLimit is a terminal connection failure: retaining more bytes would
// exceed a configured allowance. Close still owns transport/session cleanup.
var ErrBufferLimit = errors.New("aznet: byte buffer limit exceeded")

// BufferLimits bounds live bytes per connection in each ownership stage.
// Pending bounds received ciphertext and, separately, Queue reassembly.
// Decrypted bounds framed plaintext awaiting Read. Write bounds queued framed
// plaintext (including control frames). Retry bounds one sealed outgoing chunk.
// Encryption/decryption scratch and allocator capacity are additional, finite
// storage; these allowances are not a process heap or cloud storage quota.
type BufferLimits struct {
	Pending, Decrypted, Write, Retry int
}

// DefaultBufferLimits accommodates two maximum Blob chunks on receive, and one
// on retry. Large application writes are flushed in bounded batches.
func DefaultBufferLimits() BufferLimits {
	return BufferLimits{Pending: 8 << 20, Decrypted: 8 << 20, Write: 4 << 20, Retry: 4 << 20}
}

// WithBufferLimits replaces positive allowances. Write reserves one FIN header;
// Write and Retry must each fit a framed byte (plus encryption for Retry).
// Smaller receive allowances may reject a peer's larger chunks terminally.
func WithBufferLimits(limits BufferLimits) Option {
	return func(c *Config) {
		if limits.Pending > 0 {
			c.bufferLimits.Pending = limits.Pending
		}
		if limits.Decrypted > 0 {
			c.bufferLimits.Decrypted = limits.Decrypted
		}
		if limits.Write >= 2*FrameHeaderSize+1 {
			c.bufferLimits.Write = limits.Write
		}
		if limits.Retry >= NoiseOverhead+FrameHeaderSize+1 {
			c.bufferLimits.Retry = limits.Retry
		}
	}
}

// WithTableReadRows caps Table prefetch at 1..100 rows. The Pending byte
// allowance further reduces the page size using the maximum entity size.
func WithTableReadRows(rows int) Option {
	return func(c *Config) {
		if rows > 0 && rows <= 100 {
			c.tableReadRows = rows
		}
	}
}

func (c *Config) limits() BufferLimits {
	if c == nil || c.bufferLimits.Pending == 0 {
		return DefaultBufferLimits()
	}
	return c.bufferLimits
}

type bufferFailure struct{ err error }

func (c *Conn) overflow(stage string) error {
	c.limitFailure.CompareAndSwap(nil, &bufferFailure{err: fmt.Errorf("%w: %s", ErrBufferLimit, stage)})
	c.cancel()
	return c.limitFailure.Load().err
}
