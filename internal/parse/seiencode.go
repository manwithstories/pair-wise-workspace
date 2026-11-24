package parse

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"mediarun/internal/model"
)

// EncodeSEI writes one SEI NAL unit as a length-prefixed sample.
func EncodeSEI(uuid [16]byte, payload []byte) ([]byte, error) {
	if len(payload) > MaxSEIPayload {
		return nil, fmt.Errorf("%w: sei payload", model.ErrLimit)
	}
	var rbsp bytes.Buffer
	rbsp.WriteByte(seiTypeUserDataUnregistered)
	rbsp.WriteByte(byte(len(payload) + 16))
	rbsp.Write(uuid[:])
	rbsp.Write(payload)
	rbsp.WriteByte(0x80) // rbsp trailing bits
	nal := append([]byte{0x06}, addEmulationPrevention(rbsp.Bytes())...)
	out := make([]byte, 4+len(nal))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(nal)))
	copy(out[4:], nal)
	return out, nil
}

func seiUUIDHash(uuid [16]byte) uint32 {
	sum := sha256.Sum256(uuid[:])
	return binary.BigEndian.Uint32(sum[:4])
}

func removeEmulationPrevention(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c == 0x03 {
			zeros = 0
			continue
		}
		out = append(out, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

func addEmulationPrevention(b []byte) []byte {
	out := make([]byte, 0, len(b)+4)
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c <= 0x03 {
			out = append(out, 0x03)
			zeros = 0
		}
		out = append(out, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}
