package ingest

import (
	"encoding/binary"
	"io"
)

// MXF magic prefix marks the fixture as an MXF-flavored KLV stream.
var mxfMagic = []byte{0x06, 0x0e, 0x2b, 0x34, 'M', 'X', 'F', 0x00}

// berLen encodes an integer using the ASN.1 BER length scheme used by
// SMPTE KLV.
func berLen(n int) []byte {
	switch {
	case n < 0x80:
		return []byte{byte(n)}
	case n < 0x100:
		return []byte{0x81, byte(n)}
	case n < 0x10000:
		return []byte{0x82, byte(n >> 8), byte(n)}
	default:
		return []byte{0x83, byte(n >> 16), byte(n >> 8), byte(n)}
	}
}

func readBERLen(r io.Reader) (int, error) {
	var head [1]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, err
	}
	b := head[0]
	if b < 0x80 {
		return int(b), nil
	}
	n := int(b & 0x7f)
	if n == 0 || n > 4 {
		return 0, io.ErrUnexpectedEOF
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, err
	}
	v := 0
	for _, x := range buf {
		v = v<<8 | int(x)
	}
	return v, nil
}

// klw is a small KLV item writer.
type klw struct{ w io.Writer }

func (k klw) item(key []byte, payload []byte) {
	_, _ = k.w.Write(key)
	_, _ = k.w.Write(berLen(len(payload)))
	_, _ = k.w.Write(payload)
}

func localKey(tag MXFTag) []byte {
	return []byte{0x05, 0x01, byte(tag >> 8), byte(tag)}
}

func putU32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func putU64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func putPair(a, b uint32) []byte {
	out := make([]byte, 8)
	binary.BigEndian.PutUint32(out[0:], a)
	binary.BigEndian.PutUint32(out[4:], b)
	return out
}

func putCrop(c Crop) []byte {
	out := make([]byte, 16)
	binary.BigEndian.PutUint32(out[0:], uint32(c.X))
	binary.BigEndian.PutUint32(out[4:], uint32(c.Y))
	binary.BigEndian.PutUint32(out[8:], uint32(c.Width))
	binary.BigEndian.PutUint32(out[12:], uint32(c.Height))
	return out
}
