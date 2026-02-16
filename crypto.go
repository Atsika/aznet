package aznet

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/flynn/noise"
)

// NoiseOverhead is the encryption overhead: 4 bytes length prefix + 16 bytes AES-GCM tag.
const NoiseOverhead = 4 + 16

// defaultCipherSuite is the Noise cipher suite used for all connections.
// Cached at package level since it's immutable and reusable.
var defaultCipherSuite = noise.NewCipherSuite(noise.DH25519, noise.CipherAESGCM, noise.HashSHA256)

var (
	// ErrHandshakeFailed is returned when the Noise handshake fails.
	ErrHandshakeFailed = errors.New("handshake failed")
	// ErrHandshakeIncomplete is returned when the handshake is not complete.
	ErrHandshakeIncomplete = errors.New("handshake not complete")
	// ErrDecryptionFailed is returned when received data cannot be decrypted.
	ErrDecryptionFailed = errors.New("decryption failed")
	// ErrEncryptionFailed is returned when data cannot be encrypted.
	ErrEncryptionFailed = errors.New("encryption failed")
	// ErrNoiseInitFailed is returned when the Noise protocol state cannot be initialized.
	ErrNoiseInitFailed = errors.New("noise handshake initialization failed")
	// ErrNoiseMsgFailed is returned when a Noise handshake message cannot be created.
	ErrNoiseMsgFailed = errors.New("handshake message creation failed")
)

// Noise encapsulates the Noise Protocol handshake state and cipher suite.
type Noise struct {
	hs          *noise.HandshakeState
	cs1         *noise.CipherState
	cs2         *noise.CipherState
	isComplete  bool
	isInitiator bool
}

// NewNoiseClient creates a new Noise Protocol handshake as the initiator (client).
// It uses the NN pattern (no static keys, anonymous connection).
func NewNoiseClient() (*Noise, error) {
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite: defaultCipherSuite,
		Pattern:     noise.HandshakeNN,
		Initiator:   true,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoiseInitFailed, err)
	}
	return &Noise{hs: hs, isInitiator: true}, nil
}

// NewNoiseServer creates a new Noise Protocol handshake as the responder (server).
// It uses the NN pattern (no static keys, anonymous connection).
func NewNoiseServer() (*Noise, error) {
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite: defaultCipherSuite,
		Pattern:     noise.HandshakeNN,
		Initiator:   false,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoiseInitFailed, err)
	}
	return &Noise{hs: hs, isInitiator: false}, nil
}

// WriteMessage creates the next handshake message, encrypting the payload.
// It returns the message to send to the peer.
func (nh *Noise) WriteMessage(payload []byte) ([]byte, error) {
	msg, cs1, cs2, err := nh.hs.WriteMessage(nil, payload)
	if err != nil {
		return nil, err
	}
	// If both cipher states are returned, handshake is complete
	if cs1 != nil && cs2 != nil {
		nh.cs1, nh.cs2 = cs1, cs2
		nh.isComplete = true
	}
	return msg, nil
}

// ReadMessage processes a handshake message from the peer, decrypting the payload.
// It returns the decrypted payload.
func (nh *Noise) ReadMessage(msg []byte) ([]byte, error) {
	payload, cs1, cs2, err := nh.hs.ReadMessage(nil, msg)
	if err != nil {
		return nil, err
	}
	// If both cipher states are returned, handshake is complete
	if cs1 != nil && cs2 != nil {
		nh.cs1, nh.cs2 = cs1, cs2
		nh.isComplete = true
	}
	return payload, nil
}

// IsComplete returns true if the handshake is complete and session keys are established.
func (nh *Noise) IsComplete() bool {
	return nh.isComplete
}

// IsInitiator returns true if the handshake is the initiator.
func (nh *Noise) IsInitiator() bool {
	return nh.isInitiator
}

// GetCipherStates returns the established cipher states for encrypting/decrypting data.
// send is for sending, recv is for receiving.
func (nh *Noise) GetCipherStates() (send, recv *noise.CipherState, err error) {
	if !nh.isComplete {
		return nil, nil, ErrHandshakeIncomplete
	}
	return nh.cs1, nh.cs2, nil
}

// EncryptData encrypts application data using the established session cipher.
func (nh *Noise) EncryptData(dst, plaintext []byte) ([]byte, error) {
	if nh.isInitiator {
		return nh.cs1.Encrypt(dst, nil, plaintext)
	}
	return nh.cs2.Encrypt(dst, nil, plaintext)
}

// DecryptData decrypts application data using the established session cipher.
func (nh *Noise) DecryptData(dst, ciphertext []byte) ([]byte, error) {
	if nh.isInitiator {
		return nh.cs2.Decrypt(dst, nil, ciphertext)
	}
	return nh.cs1.Decrypt(dst, nil, ciphertext)
}

// SealData encrypts plaintext and prepends a 4-byte big-endian length.
// It uses the provided dst buffer if it has enough capacity.
func (nh *Noise) SealData(dst, plaintext []byte) ([]byte, error) {
	needed := 4 + len(plaintext) + 16 // 4 length + data + 16 tag
	if cap(dst) < needed {
		dst = make([]byte, 4, needed)
	} else {
		dst = dst[:4]
	}

	ciphertext, err := nh.EncryptData(dst[4:4], plaintext)
	if err != nil {
		return nil, err
	}

	binary.BigEndian.PutUint32(dst[:4], uint32(len(ciphertext)))
	return dst[:4+len(ciphertext)], nil
}

// UnsealData attempts to extract and decrypt a Noise chunk from data.
// It returns the decrypted plaintext into dst, the remaining data, and an error.
func (nh *Noise) UnsealData(dst, data []byte) (plaintext, remaining []byte, err error) {
	if len(data) < 4 {
		return nil, data, io.ErrShortBuffer
	}

	length := int(binary.BigEndian.Uint32(data[:4]))
	if len(data) < 4+length {
		return nil, data, io.ErrShortBuffer
	}

	decrypted, err := nh.DecryptData(dst[:0], data[4:4+length])
	if err != nil {
		return nil, nil, err
	}

	return decrypted, data[4+length:], nil
}
