package parser

import "encoding/binary"

type tableReader struct {
	src *Source
	b   *Box
	off int64
	end int64
}

func newTableReader(src *Source, b *Box) *tableReader {
	return &tableReader{src: src, b: b, off: b.PayloadOffset, end: b.End}
}

func (r *tableReader) remaining() int64 { return r.end - r.off }

func (r *tableReader) take(n int) ([]byte, bool) {
	if n < 0 || r.off < 0 || int64(n) > r.end-r.off {
		return nil, false
	}
	buf := make([]byte, n)
	if err := readExactAt(r.src.ReaderAt, buf, r.off); err != nil {
		return nil, false
	}
	r.off += int64(n)
	return buf, true
}

func (r *tableReader) u8() (byte, bool) {
	b, ok := r.take(1)
	if !ok {
		return 0, false
	}
	return b[0], true
}

func (r *tableReader) u16() (uint16, bool) {
	b, ok := r.take(2)
	if !ok {
		return 0, false
	}
	return binary.BigEndian.Uint16(b), true
}

func (r *tableReader) u32() (uint32, bool) {
	b, ok := r.take(4)
	if !ok {
		return 0, false
	}
	return binary.BigEndian.Uint32(b), true
}

func (r *tableReader) u64() (uint64, bool) {
	b, ok := r.take(8)
	if !ok {
		return 0, false
	}
	return binary.BigEndian.Uint64(b), true
}
