package cmaf

import "encoding/binary"

// reader is a small bounds-checked big-endian cursor over a box payload.
// Every short read becomes a BoxError anchored at the first missing byte.
type reader struct {
	data []byte
	pos  int
	base int64
	typ  string
	err  error
}

func newReader(data []byte, base int64) *reader {
	return &reader{data: data, base: base}
}

func (r *reader) fullBox(typ string) (version byte, flags uint32, err error) {
	r.typ = typ
	if len(r.data)-r.pos < 4 {
		return 0, 0, &BoxError{Offset: r.base + int64(r.pos), BoxType: typ,
			Reason: "truncated full-box header"}
	}
	version = r.data[r.pos]
	flags = uint32(r.data[r.pos+1])<<16 | uint32(r.data[r.pos+2])<<8 | uint32(r.data[r.pos+3])
	r.pos += 4
	return version, flags, nil
}

func (r *reader) need(n int) bool {
	if r.err != nil {
		return false
	}
	if len(r.data)-r.pos < n {
		r.err = &BoxError{Offset: r.base + int64(r.pos), BoxType: r.typ,
			Reason: "truncated box field"}
		return false
	}
	return true
}

func (r *reader) u32() (uint32, error) {
	if !r.need(4) {
		return 0, r.err
	}
	v := binary.BigEndian.Uint32(r.data[r.pos:])
	r.pos += 4
	return v, nil
}

func (r *reader) i32() (int32, error) {
	v, err := r.u32()
	return int32(v), err
}

func (r *reader) u64() (uint64, error) {
	if !r.need(8) {
		return 0, r.err
	}
	v := binary.BigEndian.Uint64(r.data[r.pos:])
	r.pos += 8
	return v, nil
}

// compositionOffset reads a v0 unsigned / v1 signed composition offset.
func (r *reader) compositionOffset(version byte) (int64, error) {
	if !r.need(4) {
		return 0, r.err
	}
	v := binary.BigEndian.Uint32(r.data[r.pos:])
	r.pos += 4
	if version == 1 {
		return int64(int32(v)), nil
	}
	return int64(v), nil
}
