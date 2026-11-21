package fixture

import (
	"encoding/binary"
)

// Byte-level box helpers. Fixtures are authored here so that the demuxer can
// be validated against byte-exact, locally generated media with no external
// assets.

func be16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
func be32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func be64(v uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b }
func be16s(v int16) []byte { return be16(uint16(v)) }
func be32s(v int32) []byte { return be32(uint32(v)) }
func zeros(n int) []byte   { return make([]byte, n) }

// box assembles a standard 32-bit-sized box.
func box(typ string, parts ...[]byte) []byte {
	if len(typ) != 4 {
		panic("box type must be 4 bytes: " + typ)
	}
	payload := join(parts...)
	out := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(out, uint32(8+len(payload)))
	copy(out[4:], typ)
	return append(out, payload...)
}

// fullbox assembles a full box carrying version and 24-bit flags.
func fullbox(typ string, version byte, flags uint32, parts ...[]byte) []byte {
	hdr := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return box(typ, append([][]byte{hdr}, parts...)...)
}

// largeBox assembles a box using a 64-bit largesize header.
func largeBox(typ string, parts ...[]byte) []byte {
	payload := join(parts...)
	out := make([]byte, 16, 16+len(payload))
	binary.BigEndian.PutUint32(out, 1)
	copy(out[4:], typ)
	binary.BigEndian.PutUint64(out[8:], uint64(16+len(payload)))
	return append(out, payload...)
}

func join(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func concat(parts ...[]byte) []byte { return join(parts...) }

// matrixForRotation builds the 3x3 fixed-point tkhd matrix for a clockwise
// rotation in {0,90,180,270}.
func matrixForRotation(deg int) []int32 {
	const one = int32(1) << 16
	switch ((deg % 360) + 360) % 360 {
	case 90:
		return []int32{0, one, 0, -one, 0, 0, 0, 0, 0x40000000}
	case 180:
		return []int32{-one, 0, 0, 0, -one, 0, 0, 0, 0x40000000}
	case 270:
		return []int32{0, -one, 0, one, 0, 0, 0, 0, 0x40000000}
	default:
		return []int32{one, 0, 0, 0, one, 0, 0, 0, 0x40000000}
	}
}

// rotationFromMatrix recovers clockwise degrees from a tkhd matrix.
func rotationFromMatrix(m [9]int32) int {
	// atan2(b, a) with 16.16 fixed point, snapped to right angles.
	a, b := int64(m[0]), int64(m[1])
	switch {
	case a == 0 && b > 0:
		return 90
	case a == 0 && b < 0:
		return 270
	case a < 0 && b == 0:
		return 180
	}
	return 0
}

func matrixParts(m []int32) [][]byte {
	out := make([][]byte, len(m))
	for i, v := range m {
		out[i] = be32s(v)
	}
	return out
}

// avcCBlob is a fixed, structurally valid AVC decoder configuration record.
// No decoder ever interprets it; it exists so demux/remux can round-trip the
// private configuration bytes verbatim.
func avcCBlob() []byte {
	sps := []byte{0x67, 0x64, 0x00, 0x1f, 0xac, 0xd9, 0x40, 0x50, 0x05, 0xbb,
		0x01, 0x10, 0x00, 0x00, 0x03, 0x00, 0x10, 0x00, 0x00, 0x03, 0x03, 0xc0, 0xf1, 0x83, 0x19, 0x60}
	pps := []byte{0x68, 0xeb, 0xe3, 0xcb, 0x22, 0xc0}
	out := []byte{1, 0x64, 0x00, 0x1f, 0xff, 0xe1}
	out = append(out, be16(uint16(len(sps)))...)
	out = append(out, sps...)
	out = append(out, 1)
	out = append(out, be16(uint16(len(pps)))...)
	out = append(out, pps...)
	return out
}
