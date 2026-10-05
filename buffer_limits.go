package aznet

import (
	"fmt"
)

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
