package pack

import (
	"encoding/binary"

	"github.com/abema/go-mp4"
)

type builder struct {
	w   *mp4.Writer
	err error
}

func (b *builder) begin(typ string) {
	if b.err != nil {
		return
	}
	_, b.err = b.w.StartBox(&mp4.BoxInfo{Type: mp4.StrToBoxType(typ)})
}

func (b *builder) end() {
	if b.err != nil {
		return
	}
	_, b.err = b.w.EndBox()
}

func (b *builder) raw(p []byte) {
	if b.err != nil {
		return
	}
	_, b.err = b.w.Write(p)
}

func (b *builder) u16(v uint16) {
	var buf [2]byte
	binary.BigEndian.PutUint16(buf[:], v)
	b.raw(buf[:])
}

func (b *builder) u32(v uint32) {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], v)
	b.raw(buf[:])
}

func (b *builder) full(typ string, flags uint32) {
	b.begin(typ)
	b.raw([]byte{0, byte(flags >> 16), byte(flags >> 8), byte(flags)})
}

func writeMatrix(b *builder) {
	for _, v := range []uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000} {
		b.u32(v)
	}
}
