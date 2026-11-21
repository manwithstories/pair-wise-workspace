package ebml

import (
	"encoding/binary"
	"math"

	"mkvarchive/internal/model"
)

// DecodeUnsigned reads a big-endian unsigned integer from payload.
func DecodeUnsigned(data []byte) uint64 {
	var v uint64
	for _, b := range data {
		v = (v << 8) | uint64(b)
	}
	return v
}

// DecodeSigned reads a big-endian signed integer (EBML signed form):
// decode the bytes as unsigned, then subtract 2^(8*len) when the MSB is set.
func DecodeSigned(data []byte) int64 {
	if len(data) == 0 {
		return 0
	}
	u := DecodeUnsigned(data)
	if data[0]&0x80 != 0 {
		return int64(u) - int64(1)<<uint(8*len(data))
	}
	return int64(u)
}

// DecodeFloat reads an IEEE-754 single or double precision float.
func DecodeFloat(data []byte) (float64, error) {
	switch len(data) {
	case 4:
		bits := binary.BigEndian.Uint32(data)
		return float64(math.Float32frombits(bits)), nil
	case 8:
		bits := binary.BigEndian.Uint64(data)
		return math.Float64frombits(bits), nil
	case 0:
		return 0, nil
	default:
		return 0, model.NewError("EBML_BAD_FLOAT", "float width must be 0, 4 or 8 bytes", -1)
	}
}

// EncodeUnsigned returns the minimal big-endian byte representation used
// inside Matroska unsigned-integer elements.
func EncodeUnsigned(v uint64) []byte {
	if v == 0 {
		return []byte{0}
	}
	var buf [8]byte
	n := 0
	for x := v; x > 0; x >>= 8 {
		buf[7-n] = byte(x & 0xFF)
		n++
	}
	return buf[8-n:]
}

// EncodeFloat always emits an 8-byte IEEE-754 double for deterministic
// re-encodability.
func EncodeFloat(v float64) []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, math.Float64bits(v))
	return buf
}
