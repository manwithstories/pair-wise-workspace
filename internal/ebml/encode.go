package ebml

import (
	"encoding/binary"
	"math"
)

// Encoder builds EBML byte sequences in memory.  It is used by the fixture
// generator and tests; the archive pipeline itself never rewrites sources.
type Encoder struct {
	buf []byte
}

// Bytes returns the accumulated bytes.
func (e *Encoder) Bytes() []byte { return e.buf }

// Len returns the accumulated length.
func (e *Encoder) Len() int { return len(e.buf) }

// Raw appends arbitrary bytes.
func (e *Encoder) Raw(b ...byte) *Encoder {
	e.buf = append(e.buf, b...)
	return e
}

// RawBytes appends arbitrary bytes.
func (e *Encoder) RawBytes(b []byte) *Encoder {
	e.buf = append(e.buf, b...)
	return e
}

// ID appends an element id in its canonical encoding.
func (e *Encoder) ID(id ID) *Encoder {
	switch {
	case id > 0xFFFFFF:
		e.buf = append(e.buf, byte(id>>24), byte(id>>16), byte(id>>8), byte(id))
	case id > 0xFFFF:
		e.buf = append(e.buf, byte(id>>16), byte(id>>8), byte(id))
	case id > 0xFF:
		e.buf = append(e.buf, byte(id>>8), byte(id))
	default:
		e.buf = append(e.buf, byte(id))
	}
	return e
}

// Header appends id plus a known size field.
func (e *Encoder) Header(id ID, size uint64) *Encoder {
	e.ID(id)
	e.buf = append(e.buf, EncodeSize(size, 0)...)
	return e
}

// UnknownHeader appends id plus an unknown size field of the given width.
func (e *Encoder) UnknownHeader(id ID, width int) *Encoder {
	e.ID(id)
	e.buf = append(e.buf, EncodeUnknownSize(width)...)
	return e
}

// Uint appends an unsigned integer element sized to the value.
func (e *Encoder) Uint(id ID, v uint64) *Encoder {
	n := 1
	for n < 8 && v >= 1<<(8*uint(n)) {
		n++
	}
	e.Header(id, uint64(n))
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], v)
	e.buf = append(e.buf, tmp[8-n:]...)
	return e
}

// Int appends a two's-complement signed integer element.
func (e *Encoder) Int(id ID, v int64) *Encoder {
	n := 1
	for n < 8 {
		lo := -(int64(1) << (8*uint(n) - 1))
		hi := (int64(1) << (8*uint(n) - 1)) - 1
		if v >= lo && v <= hi {
			break
		}
		n++
	}
	e.Header(id, uint64(n))
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], uint64(v))
	e.buf = append(e.buf, tmp[8-n:]...)
	return e
}

// Float appends a 64-bit float element.
func (e *Encoder) Float(id ID, v float64) *Encoder {
	e.Header(id, 8)
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], math.Float64bits(v))
	e.buf = append(e.buf, tmp[:]...)
	return e
}

// String appends a string element.
func (e *Encoder) String(id ID, s string) *Encoder {
	e.Header(id, uint64(len(s)))
	e.buf = append(e.buf, s...)
	return e
}

// Binary appends a binary element.
func (e *Encoder) Binary(id ID, b []byte) *Encoder {
	e.Header(id, uint64(len(b)))
	e.buf = append(e.buf, b...)
	return e
}

// Master appends a known-size master element whose payload is built by fn.
func (e *Encoder) Master(id ID, fn func(*Encoder)) *Encoder {
	sub := &Encoder{}
	fn(sub)
	return e.MasterBytes(id, sub.buf)
}

// UnknownMaster appends an unknown-size master element built by fn.
func (e *Encoder) UnknownMaster(id ID, width int, fn func(*Encoder)) *Encoder {
	sub := &Encoder{}
	fn(sub)
	e.UnknownHeader(id, width)
	e.buf = append(e.buf, sub.buf...)
	return e
}

// MasterBytes appends a known-size master element with the given payload.
func (e *Encoder) MasterBytes(id ID, payload []byte) *Encoder {
	e.Header(id, uint64(len(payload)))
	e.buf = append(e.buf, payload...)
	return e
}

// Void appends a Void element of the requested total size (header included),
// minimum 2 bytes.
func (e *Encoder) Void(total int) *Encoder {
	if total < 2 {
		total = 2
	}
	e.ID(IDVoid)
	e.buf = append(e.buf, EncodeSize(uint64(total-2), 0)...)
	e.buf = append(e.buf, make([]byte, total-2-len(EncodeSize(uint64(total-2), 0)))...)
	return e
}

// EncodeBlockHeader builds a SimpleBlock/Block header: track vint, 16-bit
// relative timestamp, flags and optional lacing descriptors.
func EncodeBlockHeader(track uint64, rel int16, frames int, lace string, sizes []int64) []byte {
	enc := &Encoder{}
	enc.buf = append(enc.buf, EncodeSize(track, 0)...)
	var relb [2]byte
	binary.BigEndian.PutUint16(relb[:], uint16(rel))
	enc.buf = append(enc.buf, relb[:]...)
	flags := byte(0)
	switch lace {
	case "xiph":
		flags |= 0x02
	case "fixed":
		flags |= 0x04
	case "ebml":
		flags |= 0x06
	}
	flags |= 0x80 // keyframe bit (SimpleBlock only)
	enc.buf = append(enc.buf, flags)
	switch {
	case frames > 1 && lace == "xiph":
		enc.buf = append(enc.buf, byte(frames-1))
		for _, s := range sizes[:len(sizes)-1] {
			v := s
			for v >= 0xFF {
				enc.buf = append(enc.buf, 0xFF)
				v -= 0xFF
			}
			enc.buf = append(enc.buf, byte(v))
		}
	case frames > 1 && lace == "ebml":
		enc.buf = append(enc.buf, byte(frames-1))
		enc.buf = append(enc.buf, EncodeSize(uint64(sizes[0]), 0)...)
		for i := 1; i < frames-1; i++ {
			enc.buf = append(enc.buf, EncodeSignedVINT(sizes[i]-sizes[i-1])...)
		}
	case frames > 1:
		enc.buf = append(enc.buf, byte(frames-1))
	}
	return enc.buf
}

// EncodeSignedVINT encodes an EBML-lacing signed vint in minimal width.
func EncodeSignedVINT(v int64) []byte {
	width := 1
	for width < 8 {
		bias := int64(1)<<(7*uint(width)-1) - 1
		if v >= -bias && v <= bias {
			break
		}
		width++
	}
	bias := int64(1)<<(7*uint(width)-1) - 1
	raw := uint64(v + bias)
	out := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		out[i] = byte(raw)
		raw >>= 8
	}
	out[0] |= 0x80 >> uint(width-1)
	return out
}
