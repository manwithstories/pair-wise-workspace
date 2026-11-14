// Package fixture synthesizes self-contained MP4 test files. go-mp4 is used
// here and only here, strictly for leaf-box payload serialization; every box
// header and container is written by this package and parsed back by the
// self-developed main pipeline.
package fixture

import (
	"bytes"
	"encoding/binary"
)

// builder accumulates one box payload with big-endian helpers.
type builder struct{ bytes.Buffer }

func (b *builder) u8(v uint8)   { b.WriteByte(v) }
func (b *builder) u16(v uint16) { var x [2]byte; binary.BigEndian.PutUint16(x[:], v); b.Write(x[:]) }
func (b *builder) u24(v uint32) { b.Write([]byte{byte(v >> 16), byte(v >> 8), byte(v)}) }
func (b *builder) u32(v uint32) { var x [4]byte; binary.BigEndian.PutUint32(x[:], v); b.Write(x[:]) }
func (b *builder) u64(v uint64) { var x [8]byte; binary.BigEndian.PutUint64(x[:], v); b.Write(x[:]) }
func (b *builder) i16(v int16)  { b.u16(uint16(v)) }
func (b *builder) i32(v int32)  { b.u32(uint32(v)) }
func (b *builder) i64(v int64)  { b.u64(uint64(v)) }
func (b *builder) raw(p []byte) { b.Write(p) }

// fullbox prefixes version and 24-bit flags.
func (b *builder) fullbox(ver uint8, flags uint32) {
	b.u8(ver)
	b.u24(flags)
}

func (b *builder) fourcc(s string) {
	var x [4]byte
	copy(x[:], s)
	b.Write(x[:])
}

// box wraps a payload into a complete box (header + body), choosing 64-bit
// size only when needed.
func box(typ string, payload []byte) []byte {
	n := uint64(8 + len(payload))
	var hdr bytes.Buffer
	if n > 0xFFFFFFFF {
		hdr.Write([]byte{0, 0, 0, 1})
		hdr.WriteString(typ)
		var x [8]byte
		binary.BigEndian.PutUint64(x[:], n)
		hdr.Write(x[:])
	} else {
		var x [4]byte
		binary.BigEndian.PutUint32(x[:], uint32(n))
		hdr.Write(x[:])
		hdr.WriteString(typ)
	}
	out := make([]byte, 0, n)
	out = append(out, hdr.Bytes()...)
	out = append(out, payload...)
	return out
}

// container joins already-serialized child boxes.
func container(typ string, children ...[]byte) []byte {
	var body bytes.Buffer
	for _, c := range children {
		body.Write(c)
	}
	return box(typ, body.Bytes())
}
