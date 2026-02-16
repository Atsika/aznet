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
