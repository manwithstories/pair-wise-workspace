package fixture

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// boxBytes builds one ISO-BMFF box. The extended-size form is used when the
// payload would overflow a 32-bit size field.
func boxBytes(typ string, payload []byte) []byte {
	size := len(payload) + 8
	var b bytes.Buffer
	if size > 0x7fffffff {
		binary.Write(&b, binary.BigEndian, uint32(1))
		b.WriteString(typ)
		binary.Write(&b, binary.BigEndian, uint64(size+8))
	} else {
		binary.Write(&b, binary.BigEndian, uint32(size))
		b.WriteString(typ)
	}
	b.Write(payload)
	return b.Bytes()
}

func fullBox(typ string, version uint8, flags uint32, payload []byte) []byte {
	hdr := make([]byte, 4)
	hdr[0] = version
	hdr[1] = byte(flags >> 16)
	hdr[2] = byte(flags >> 8)
	hdr[3] = byte(flags)
	return boxBytes(typ, append(hdr, payload...))
}

func concat(parts ...[]byte) []byte {
	var b bytes.Buffer
	for _, p := range parts {
		b.Write(p)
	}
	return b.Bytes()
}

// w is a small big-endian writer with a sticky error.
type w struct {
	buf bytes.Buffer
	err error
}

func (x *w) u8(v uint8)   { x.buf.WriteByte(v) }
func (x *w) u16(v uint16) { binary.Write(&x.buf, binary.BigEndian, v) }
func (x *w) u24(v uint32) {
	x.u8(uint8(v >> 16))
	x.u8(uint8(v >> 8))
	x.u8(uint8(v))
}
func (x *w) u32(v uint32)  { binary.Write(&x.buf, binary.BigEndian, v) }
func (x *w) u64(v uint64)  { binary.Write(&x.buf, binary.BigEndian, v) }
func (x *w) i32(v int32)   { binary.Write(&x.buf, binary.BigEndian, v) }
func (x *w) raw(p []byte)  { x.buf.Write(p) }
func (x *w) bytes() []byte { return x.buf.Bytes() }

func (x *w) errf(format string, args ...any) {
	if x.err == nil {
		x.err = fmt.Errorf(format, args...)
	}
}

// ftyp writes a minimal isom/mp41 file type.
func ftyp() []byte {
	x := &w{}
	x.raw([]byte("isom"))
	x.u32(512)
	x.raw([]byte("isom"))
	x.raw([]byte("iso2"))
	x.raw([]byte("mp41"))
	return boxBytes("ftyp", x.bytes())
}

// matrix is the identity transform stored in tkhd/mvhd.
func identityMatrix(x *w) {
	for _, v := range []uint32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000} {
		x.u32(v)
	}
}
