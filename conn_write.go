package aznet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// pendingChunk holds a sealed chunk whose write failed, for verbatim resend.
// Re-sealing is not an option: Noise nonces advance per seal, so a second seal
// of the same plaintext would desync the peer permanently. Guarded by flushGate.
type pendingChunk struct {
	data    []byte // sealed ciphertext, owned copy
	seq     uint64 // chunk sequence, reused so the retry is idempotent
	consume int    // bytes of bufs.Write this chunk covers (0 for control chunks)
	rotate  bool   // call RotateTX once the write lands
	valid   bool   // data holds a chunk still owed to the peer
	sent    bool   // ciphertext landed, but rotation may still need committing
}

// Write reports bytes accepted into the connection, including when flushing
// fails. The connection owns those bytes and retains their ciphertext for retry;
// callers must only resubmit p[n:].
func (c *Conn) Write(p []byte) (int, error) {
	if c.closed.Load() == 1 || c.closedWrite.Load() == 1 {
		return 0, io.ErrClosedPipe
	}
	ctx, finish := c.writeDeadline.operation(c.ctx)
	defer finish()
	if err := context.Cause(ctx); err != nil {
		return 0, c.ioError(ctx, err)
	}

	if err := lockIO(ctx, c.writeGate); err != nil {
		return 0, c.ioError(ctx, err)
	}
	defer func() { <-c.writeGate }()
	if len(p) == 0 {
		return 0, c.ioError(ctx, c.flushContext(ctx))
	}
	accepted := 0
	for len(p) > 0 {
		c.wmu.Lock()
		if c.bufs == nil || c.closed.Load() == 1 || c.closedWrite.Load() == 1 {
			c.wmu.Unlock()
			return accepted, io.ErrClosedPipe
		}
		if c.mtu <= 0 {
			c.wmu.Unlock()
			return accepted, c.overflow("transport capacity")
		}
		// Reserve a FIN header so shutdown never needs to exceed the allowance.
		available := c.cfg.limits().Write - FrameHeaderSize - c.bufs.Write.Len()
		for len(p) > 0 && available > FrameHeaderSize {
			size := min(len(p), c.mtu)
			if size+FrameHeaderSize > available {
				break
			}
			BuildFrame(&c.bufs.Write, Frame{Type: MsgTypeData, Payload: p[:size]})
			p = p[size:]
			accepted += size
			available -= size + FrameHeaderSize
		}
		c.wmu.Unlock()
		if err := c.flushContext(ctx); err != nil {
			return accepted, c.ioError(ctx, err)
		}
	}
	return accepted, nil
}

// CloseWrite shuts down the writing side of the connection. It sends a FIN frame
// to the peer to indicate that no more data will be sent.
func (c *Conn) CloseWrite() error {
	if c.closed.Load() == 1 || c.closedWrite.Swap(1) == 1 {
		return nil
	}
	c.wmu.Lock()
	if c.bufs == nil {
		c.wmu.Unlock()
		return net.ErrClosed
	}
	BuildFrame(&c.bufs.Write, Frame{Type: MsgTypeFin})
	c.wmu.Unlock()

	return c.flush()
}

// keepAlive sends a Ping frame whenever nothing has been flushed for a full
// pingInterval.
func (c *Conn) keepAlive() {
	ticker := time.NewTicker(c.cfg.pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if c.closed.Load() == 1 || c.closedWrite.Load() == 1 {
				return
			}
			last := c.lastActive.Load()
			if time.Since(time.Unix(0, last)) >= c.cfg.pingInterval {
				c.wmu.Lock()
				if c.bufs == nil || c.closed.Load() == 1 || c.closedWrite.Load() == 1 {
					c.wmu.Unlock()
					return
				}
				if c.bufs.Write.Len()+2*FrameHeaderSize <= c.cfg.limits().Write {
					BuildFrame(&c.bufs.Write, Frame{Type: MsgTypePing})
				}
				c.wmu.Unlock()
				_ = c.flush()
				continue
			}
		}
	}
}

func (c *Conn) flush() error {
	ctx, finish := c.writeDeadline.operation(c.ctx)
	defer finish()
	err := c.flushContext(ctx)
	if err != nil {
		return c.ioError(ctx, err)
	}
	return nil
}

func (c *Conn) flushContext(ctx context.Context) error {
	if err := lockIO(ctx, c.flushGate); err != nil {
		return err
	}
	defer func() { <-c.flushGate }()

	if c.bufs == nil {
		return net.ErrClosed
	}

	// Resend a failed chunk before sealing anything new, so chunks stay in nonce
	// order. Its original seq keeps the resend idempotent at the driver.
	if c.pending.valid {
		if err := c.sendChunk(ctx, c.pending.data, c.pending.consume, c.pending.rotate, c.pending.seq); err != nil {
			return err
		}
	}

	// Derived from mtu so the largest frame always fits in one chunk.
	maxChunk := int(c.mtu) + FrameHeaderSize

	for {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		c.wmu.Lock()
		if c.bufs.Write.Len() == 0 {
			c.wmu.Unlock()
			return nil
		}

		// Always at a frame boundary, since chunking below is frame-aligned.
		if c.rotator != nil && c.rotator.ShouldRotate() {
			c.wmu.Unlock()

			// Send rotation frame
			var rBuf bytes.Buffer
			BuildFrame(&rBuf, Frame{Type: MsgTypeRotate})

			sealed, err := c.noise.SealData(c.bufs.Enc, rBuf.Bytes())
			if err != nil {
				return err
			}
			c.bufs.Enc = sealed[:0]

			if err := c.sendChunk(ctx, sealed, 0, true, c.chunkSeq); err != nil {
				return err
			}
			continue // Re-check buffer after rotation
		}

		takeLen := alignedChunkLen(c.bufs.Write.Bytes(), maxChunk)
		if takeLen == 0 {
			// Framing is already broken; an unaligned chunk would hide it.
			c.wmu.Unlock()
			return fmt.Errorf("%w: frame exceeds chunk size %d", ErrFrameTooLarge, maxChunk)
		}

		// Seal while still holding wmu: the slice aliases the write buffer's
		// backing array, which a concurrent Write can slide in place.
		sealed, err := c.noise.SealData(c.bufs.Enc, c.bufs.Write.Bytes()[:takeLen])
		if err != nil {
			c.wmu.Unlock()
			return err
		}
		c.bufs.Enc = sealed[:0]
		c.wmu.Unlock()

		if err := c.sendChunk(ctx, sealed, takeLen, false, c.chunkSeq); err != nil {
			return err
		}

		c.lastActive.Store(time.Now().UnixNano())
		c.nudgeReader()
	}
}

// sendChunk writes one sealed chunk, consuming the plaintext it covered and
// applying any rotation only once the write lands; a failure leaves both queued
// for retry. Caller must hold flushGate and must not hold wmu.
func (c *Conn) sendChunk(ctx context.Context, sealed []byte, consume int, rotate bool, seq uint64) (err error) {
	if seq == ^uint64(0) {
		return c.overflow("sequence exhausted")
	}
	if len(sealed) > c.cfg.limits().Retry {
		return c.overflow("retry ciphertext")
	}
	sent := c.pending.valid && c.pending.sent
	defer func() {
		if err != nil {
			if !c.pending.valid {
				// Copy only on failure; the next encryption reuses bufs.Enc.
				c.pending = pendingChunk{data: append(c.pending.data[:0], sealed...), seq: seq, consume: consume, rotate: rotate, valid: true}
			}
			c.pending.sent = sent
		}
	}()
	if !sent {
		if err = c.transport.WriteRaw(ctx, seq, bytes.NewReader(sealed)); err != nil {
			if errors.Is(err, ErrBufferLimit) {
				return c.overflow("transport write")
			}
			return err
		}
		sent = true
	}
	if rotate {
		if err = c.rotator.RotateTX(ctx); err != nil {
			return err
		}
	}

	c.pending = pendingChunk{}
	c.chunkSeq = seq + 1

	if consume > 0 {
		c.wmu.Lock()
		c.bufs.Write.Next(consume)
		c.wmu.Unlock()
	}
	return nil
}
