// Package mp4util builds serialized ISO base media file format boxes without
// depending on a muxer. It is deterministic: identical input always yields
// identical bytes.
package mp4util

import (
	"encoding/binary"
	"fmt"
)

// U8, U16, U24, U32 and U64 encode big-endian integers.
func U8(v uint8) []byte { return []byte{v} }
func U16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}
func U24(v uint32) []byte {
	return []byte{byte(v >> 16), byte(v >> 8), byte(v)}
}
func U32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}
func U64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

// Cat concatenates byte slices.
func Cat(parts ...[]byte) []byte {
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

// Zeros returns n zero bytes.
func Zeros(n int) []byte { return make([]byte, n) }

// FourCC converts a four character code to bytes, padding with spaces.
func FourCC(s string) []byte {
	b := []byte(s)
	for len(b) < 4 {
		b = append(b, ' ')
	}
	return b[:4]
}

// Box serializes one box. The total size must fit in 32 bits; larger payloads
// are a programming error in this project and panic rather than truncate.
func Box(typ string, payload ...[]byte) []byte {
	body := Cat(payload...)
	size := uint64(len(body)) + 8
	if size > 0xFFFFFFFF {
		panic(fmt.Sprintf("mp4util: box %q too large (%d bytes)", typ, size))
	}
	return Cat(U32(uint32(size)), FourCC(typ), body)
}

// FullBox serializes a full box with version and flags.
func FullBox(typ string, version uint8, flags uint32, payload ...[]byte) []byte {
	vf := Cat(U8(version), U24(flags))
	return Box(typ, append([][]byte{vf}, payload...)...)
}

// Container serializes a box whose payload is a sequence of child boxes.
func Container(typ string, children ...[]byte) []byte { return Box(typ, children...) }

// Matrix is the identity display matrix used by mvhd and tkhd.
func Matrix() []byte {
	return Cat(
		U32(0x00010000), U32(0), U32(0),
		U32(0), U32(0x00010000), U32(0),
		U32(0), U32(0), U32(0x40000000),
	)
}

// Language packs an ISO-639-2 code into the mdhd 15-bit representation.
func Language(code string) uint16 {
	b := []byte(code)
	for len(b) < 3 {
		b = append(b, ' ')
	}
	return uint16((b[0]-0x60)&0x1F)<<10 | uint16((b[1]-0x60)&0x1F)<<5 | uint16((b[2]-0x60)&0x1F)
}
