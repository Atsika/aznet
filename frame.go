package aznet

import (
	"bytes"
	"encoding/binary"
)

const FrameHeaderSize = 4 + 1 // 4 bytes length + 1 byte type

// Frame represents a single message unit.
type Frame struct {
	Payload []byte
	Length  uint32
	Type    byte
}

// alignedChunkLen returns the length of the longest prefix of buf that holds
// only whole frames and still fits in maxChunk. buf must start on a frame
// boundary.
//
// Chunking this way is what keeps rotation safe. flush() checks for rotation
// between chunks, so an unaligned chunk boundary lets the Rotate frame land in
// the middle of a data frame: the receiver reads the rotate bytes as that
// frame's payload, framing desyncs, and RotateRX never runs. For append blobs
// the two halves of the split frame would also end up on different blobs.
//
// It returns 0 only when the first frame alone exceeds maxChunk, which cannot
// happen while Write splits payloads at mtu (maxChunk - FrameHeaderSize).
func alignedChunkLen(buf []byte, maxChunk int) int {
	n := 0
	for n+FrameHeaderSize <= len(buf) {
		end := n + FrameHeaderSize + int(binary.BigEndian.Uint32(buf[n:n+4]))
		if end > maxChunk || end > len(buf) {
			break
		}
		n = end
	}
	return n
}

// BuildFrame writes a framed message to the write buffer.
// Frame format: [4 bytes: length][1 byte: type][N bytes: payload]
// Caller must ensure writeBuf is protected from concurrent access.
func BuildFrame(writeBuf *bytes.Buffer, f Frame) {
	writeBuf.Grow(FrameHeaderSize + len(f.Payload))
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(f.Payload)))
	writeBuf.Write(lenBuf[:])
	writeBuf.WriteByte(f.Type)
	writeBuf.Write(f.Payload)
}
