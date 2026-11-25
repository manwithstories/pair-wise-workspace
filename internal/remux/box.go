// Package remux writes recovered coded samples back into ISO-BMFF archive
// fragments without re-encoding. Sample bytes are copied verbatim; only the
// container boxes (ftyp/moov/mdat and the sample tables) are regenerated.
package remux

import (
	"encoding/binary"
	"io"
)

// boxBuilder accumulates one ISO-BMFF box. Nested boxes are appended in order;
// sizes are computed bottom-up at Serialize time.
type boxBuilder struct {
	typ      string
	header   []byte // full-box version/flags or fixed header fields, no box header
	payload  []byte // raw payload for leaf boxes
	children []boxBuilder
}

func newBox(typ string) *boxBuilder { return &boxBuilder{typ: typ} }

func fullHeader(version byte, flags uint32) []byte {
	return []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
}

func (b *boxBuilder) setRaw(p []byte) *boxBuilder { b.payload = p; return b }
func (b *boxBuilder) setHeader(h []byte) *boxBuilder { b.header = h; return b }
func (b *boxBuilder) add(child boxBuilder) *boxBuilder {
	b.children = append(b.children, child)
	return b
}

func (b *boxBuilder) bodyLen() int {
	n := len(b.header) + len(b.payload)
	for i := range b.children {
		n += 8 + b.children[i].bodyLen()
	}
	return n
}

// serialize writes the box with a correct 8-byte size header. All fixture
// boxes fit well inside uint32, so large-box encoding is not needed.
func (b *boxBuilder) serialize(w io.Writer) (int64, error) {
	total := 8 + b.bodyLen()
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr, uint32(total))
	copy(hdr[4:8], b.typ)
	n, err := w.Write(hdr)
	if err != nil {
		return int64(n), err
	}
	written := int64(n)
	if len(b.header) > 0 {
		n, err = w.Write(b.header)
		written += int64(n)
		if err != nil {
			return written, err
		}
	}
	if len(b.payload) > 0 {
		n, err = w.Write(b.payload)
		written += int64(n)
		if err != nil {
			return written, err
		}
	}
	for i := range b.children {
		cn, err := b.children[i].serialize(w)
		written += cn
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func putU32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }
func putU16(b []byte, v uint16) { binary.BigEndian.PutUint16(b, v) }

// u32Box builds a full box whose payload is count followed by n u32 values.
func u32Box(typ string, vals []uint32) boxBuilder {
	body := make([]byte, 8+4*len(vals))
	putU32(body[4:8], uint32(len(vals)))
	for i, v := range vals {
		putU32(body[8+4*i:], v)
	}
	return boxBuilder{typ: typ, header: body[:4], payload: body[4:]}
}

// u64Box builds co64: count followed by n u64 chunk offsets.
func u64Box(typ string, vals []int64) boxBuilder {
	body := make([]byte, 8+8*len(vals))
	putU32(body[4:8], uint32(len(vals)))
	for i, v := range vals {
		binary.BigEndian.PutUint64(body[8+8*i:], uint64(v))
	}
	return boxBuilder{typ: typ, header: body[:4], payload: body[4:]}
}
