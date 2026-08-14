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
// boundary. Keeping chunks frame-aligned means a rotation between chunks can
// never split a frame. Returns 0 if the first frame alone exceeds maxChunk.
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
