package aznet

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"
)

func (c *Conn) Read(p []byte) (n int, err error) {
	ctx, finish := c.readDeadline.operation(c.ctx)
	defer finish()
	defer func() {
		if err != nil {
			err = c.ioError(ctx, err)
		}
	}()
	if err := lockIO(ctx, c.readGate); err != nil {
		return 0, err
	}
	defer func() { <-c.readGate }()
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if c.closed.Load() == 1 {
			return 0, net.ErrClosed
		}

		c.rmu.Lock()
		if c.closedRead.Load() == 1 {
			c.rmu.Unlock()
			return 0, io.EOF
		}
		// Close() recycles bufs into the shared pool while holding rmu, so a
		// non-nil check under the lock is what keeps this read off a buffer that
		// now belongs to another connection.
		if c.bufs == nil {
			c.rmu.Unlock()
			return 0, net.ErrClosed
		}

		if err := context.Cause(ctx); err != nil {
			c.rmu.Unlock()
			return 0, err
		}

		// Drain leftover payload from a previous partial read.
		if c.readRemain > 0 {
			n := copy(p, c.bufs.Read.Next(min(c.readRemain, len(p))))
			c.readRemain -= n
			c.rmu.Unlock()
			return n, nil
		}

		// Peek at next frame header without consuming payload.
		if c.bufs.Read.Len() >= FrameHeaderSize {
			header := c.bufs.Read.Bytes()[:FrameHeaderSize]
			fType := header[4]
			fLen64 := uint64(binary.BigEndian.Uint32(header[:4]))
			if fLen64+FrameHeaderSize > uint64(c.cfg.limits().Decrypted) {
				c.rmu.Unlock()
				return 0, c.overflow("decrypted frame")
			}
			fLen := int(fLen64)

			if c.bufs.Read.Len() >= FrameHeaderSize+fLen {
				c.peerLastSeen.Store(time.Now().UnixNano())
				switch fType {
				case MsgTypeData:
					// Consume header, then read min(fLen, len(p)) from payload.
					c.bufs.Read.Next(FrameHeaderSize)
					n := copy(p, c.bufs.Read.Next(min(fLen, len(p))))
					c.readRemain = fLen - n
					c.rmu.Unlock()
					return n, nil
				case MsgTypePing:
					c.bufs.Read.Next(FrameHeaderSize + fLen)
					c.rmu.Unlock()
					continue
				case MsgTypeFin:
					c.bufs.Read.Next(FrameHeaderSize + fLen)
					c.closedRead.Store(1)
					c.rmu.Unlock()
					return 0, io.EOF
				case MsgTypeRotate:
					c.bufs.Read.Next(FrameHeaderSize + fLen)
					if c.rotator != nil {
						_ = c.rotator.RotateRX()
					}
					c.rmu.Unlock()
					continue
				default:
					c.bufs.Read.Next(FrameHeaderSize + fLen)
					c.rmu.Unlock()
					continue
				}
			}
		}

		// A previous operation may have received ciphertext before its deadline
		// interrupted decryption. Process that owned data before polling again.
		maxChunk := min(c.transport.MaxRawSize(), c.cfg.limits().Pending)
		if c.bufs.Noise.Len() >= 4 {
			sealedSize := uint64(binary.BigEndian.Uint32(c.bufs.Noise.Bytes()[:4])) + 4
			available := c.cfg.limits().Decrypted - c.bufs.Read.Len()
			if sealedSize > uint64(available)+NoiseOverhead {
				c.rmu.Unlock()
				return 0, c.overflow("decrypted chunk")
			}
		}
		decrypted, rest, decodeErr := c.noise.UnsealData(c.bufs.Dec, c.bufs.Noise.Bytes(), maxChunk)
		if decodeErr != nil && decodeErr != io.ErrShortBuffer {
			c.rmu.Unlock()
			if errors.Is(decodeErr, ErrChunkTooLarge) {
				return 0, c.overflow("ciphertext chunk")
			}
			return 0, decodeErr
		}
		if decodeErr == nil {
			c.bufs.Dec = decrypted[:0]
			c.cleanupToken.Do(func() {
				if c.session != nil {
					go c.session.deleteToken()
				}
			})
			c.bufs.Read.Write(decrypted)
			used := c.bufs.Noise.Len() - len(rest)
			c.bufs.Noise.Next(used)
			c.rmu.Unlock()
			// Drain framed plaintext before decrypting the next chunk.
			continue
		}
		receiveBudget := c.cfg.limits().Pending - c.bufs.Noise.Len()
		c.rmu.Unlock()

		// Fetch more data
		var rawStream io.ReadCloser
		var err error
		if reader, ok := c.transport.(limitedRawReader); ok {
			rawStream, err = reader.ReadRawLimit(ctx, receiveBudget)
		} else {
			rawStream, err = c.transport.ReadRaw(ctx)
		}
		if err != nil {
			if errors.Is(err, ErrBufferLimit) {
				return 0, c.overflow("transport receive")
			}
			if errors.Is(err, ErrNoData) {
				if err := c.idleWait(ctx); err != nil {
					return 0, err
				}
				continue
			}
			if errors.Is(err, context.Canceled) && c.closed.Load() == 1 {
				return 0, net.ErrClosed
			}
			return 0, err
		}

		// Read directly from the stream into the Noise buffer, then decrypt.
		// readGate covers fetching, body consumption, decryption and frame parsing.
		// rmu additionally protects buffer recycling during teardown.
		c.rmu.Lock()
		if c.bufs == nil {
			c.rmu.Unlock()
			rawStream.Close()
			return 0, net.ErrClosed
		}

		// Closing the response body interrupts a blocked stream read as well
		// as canceling the context passed to ReadRaw.
		bodyClosed := make(chan struct{})
		stopClose := context.AfterFunc(ctx, func() {
			rawStream.Close()
			close(bodyClosed)
		})
		remaining := c.cfg.limits().Pending - c.bufs.Noise.Len()
		_, err = c.bufs.Noise.ReadFrom(io.LimitReader(rawStream, int64(remaining)))
		if err == nil {
			var extra [1]byte
			n, probeErr := io.ReadFull(rawStream, extra[:])
			if n > 0 {
				err = c.overflow("pending ciphertext")
			} else if probeErr != io.EOF {
				err = probeErr
			}
		}
		if stopClose() {
			rawStream.Close()
		} else {
			<-bodyClosed
		}
		if cause := context.Cause(ctx); cause != nil {
			err = cause
		}
		if err != nil && err != io.EOF {
			c.rmu.Unlock()
			return 0, err
		}

		c.rmu.Unlock()
		c.poll.Reset()
	}
}

// nudgeReader hints the read loop that a reply is likely imminent (we just sent),
// collapsing its poll back-off to fetch sooner. Rate-limited to one nudge per
// dataPoll so a one-directional writer cannot pin the idle reverse channel at
// fast-poll rate and bill extra reads.
func (c *Conn) nudgeReader() {
	now := time.Now().UnixNano()
	last := c.lastNudge.Load()
	if now-last < int64(c.cfg.dataPoll) {
		return
	}
	if !c.lastNudge.CompareAndSwap(last, now) {
		return
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// idleWait shares the operation context so deadline updates interrupt polling.
func (c *Conn) idleWait(ctx context.Context) error {
	timer := time.NewTimer(c.poll.Next())
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-c.wake:
		c.poll.Reset()
		return nil
	case <-timer.C:
		return nil
	}
}
