package mp4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Safe binary primitives. All slices are bounded by scanned box payloads.

func appendU16(b []byte, v uint16) []byte {
	var x [2]byte
	binary.BigEndian.PutUint16(x[:], v)
	return append(b, x[:]...)
}

func appendU32(b []byte, v uint32) []byte {
	var x [4]byte
	binary.BigEndian.PutUint32(x[:], v)
	return append(b, x[:]...)
}

func appendU64(b []byte, v uint64) []byte {
	var x [8]byte
	binary.BigEndian.PutUint64(x[:], v)
	return append(b, x[:]...)
}

func appendFullBoxHeader(b []byte, version byte, flags uint32) []byte {
	b = append(b, version)
	b = append(b, byte(flags>>16), byte(flags>>8), byte(flags))
	return b
}

func boxBytes(typ string, payload []byte) []byte {
	if len(typ) != 4 {
		panic(fmt.Sprintf("mp4 box type must be four characters: %q", typ))
	}
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[:4], uint32(len(out)))
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

func fullBoxBytes(typ string, version byte, flags uint32, payload []byte) []byte {
	hdr := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return boxBytes(typ, append(hdr, payload...))
}

type byteReader struct {
	b   []byte
	pos int
}

func newByteReader(b []byte) *byteReader { return &byteReader{b: b} }

func (r *byteReader) remaining() int { return len(r.b) - r.pos }

func (r *byteReader) take(n int) ([]byte, error) {
	if n < 0 || n > r.remaining() {
		return nil, fmt.Errorf("%w: need %d bytes at %d, have %d", io.ErrUnexpectedEOF, n, r.pos, r.remaining())
	}
	out := r.b[r.pos : r.pos+n]
	r.pos += n
	return out, nil
}

func (r *byteReader) u8() (byte, error) {
	b, err := r.take(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (r *byteReader) u16() (uint16, error) {
	b, err := r.take(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b), nil
}

func (r *byteReader) u32() (uint32, error) {
	b, err := r.take(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b), nil
}

func (r *byteReader) u64() (uint64, error) {
	b, err := r.take(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(b), nil
}

func (r *byteReader) i32() (int32, error) {
	v, err := r.u32()
	return int32(v), err
}

func (r *byteReader) i64() (int64, error) {
	v, err := r.u64()
	return int64(v), err
}

// readBoxPayload seeks to a scanned box payload and returns a bounded reader.
func readBoxPayload(rs io.ReadSeeker, b *Box) (*byteReader, error) {
	if b == nil || b.Size < b.HeaderSize {
		return nil, errors.New("cannot read payload of invalid box")
	}
	if _, err := rs.Seek(b.Offset+b.HeaderSize, io.SeekStart); err != nil {
		return nil, err
	}
	size := b.Size - b.HeaderSize
	const maxMetadataPayload = 1 << 20 // custom JSON audit boxes are tiny
	if size > maxMetadataPayload {
		return nil, fmt.Errorf("metadata payload too large: %d", size)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(rs, buf); err != nil {
		return nil, fmt.Errorf("read %s payload: %w", b.Type, err)
	}
	return newByteReader(buf), nil
}
