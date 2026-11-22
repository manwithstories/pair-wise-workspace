// Package fixture generates deterministic synthetic elementary streams,
// keyframe indexes and fault injections. All data is produced in-process;
// no external files are referenced. Payloads are framing-only and are never
// meant for a real decoder.
package fixture

import (
	"encoding/binary"
)

// Magic/version shared with the index package.
const (
	IndexMagic   = "MPKIDX1"
	IndexVersion = uint8(1)
	IndexEntrySz = 16
)

// trailer mirrors codec.frameTrailerLen semantics (kept local to avoid an
// import cycle): "MPK1" + uint32 declared payload length.
func trailer(payloadLen int) []byte {
	b := make([]byte, 8)
	copy(b[:4], "MPK1")
	binary.BigEndian.PutUint32(b[4:], uint32(payloadLen))
	return b
}

func startCode(dst []byte, four bool) int {
	if four {
		copy(dst, []byte{0, 0, 0, 1})
		return 4
	}
	copy(dst, []byte{0, 0, 1})
	return 3
}

// payload builds deterministic frame bytes. Values avoid 0x000001 sequences
// so framing is unambiguous without emulation-prevention processing.
func payload(seed int64, frame int, key bool, extra int) []byte {
	n := 24 + (frame%5)*8 + extra
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((seed*7 + int64(frame*13+i*31) + 1) & 0x7F)
	}
	if key {
		b[0] |= 0x40
	}
	return b
}
