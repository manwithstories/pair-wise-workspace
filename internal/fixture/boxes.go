// Package fixture generates deterministic synthetic ISO BMFF (MP4) assets
// and the documented damage variants the pipeline must detect. Samples use
// a tiny self-describing, checksummed synthetic codec so corruption can be
// verified without any real encoder, network access or external file.
//
// Synthetic sample layout (all multi-byte integers big-endian):
//
//	0..1   magic 0x4D43 ("MC")
//	2..5   uint32 sample index, 0-based within the track
//	6..7   uint16 body length
//	8..    body bytes
//	last 4 uint32 CRC-32/IEEE over every preceding byte
package fixture

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
)

// memBuffer is an in-memory io.WriteSeeker used by unit tests; the demo
// writes to *os.File, which natively satisfies WriteSeeker and Truncate.
type memBuffer struct {
	buf bytes.Buffer
	pos int64
}

func (m *memBuffer) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if end > int64(m.buf.Len()) {
		need := end - int64(m.buf.Len())
		m.buf.Write(make([]byte, need))
	}
	copy(m.buf.Bytes()[m.pos:], p)
	m.pos = end
	return len(p), nil
}

func (m *memBuffer) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = m.pos + offset
	case io.SeekEnd:
		target = int64(m.buf.Len()) + offset
	}
	if target < 0 {
		return 0, errors.New("negative seek")
	}
	m.pos = target
	return target, nil
}

func (m *memBuffer) Truncate(size int64) error {
	if size < 0 || size > int64(m.buf.Len()) {
		return errors.New("bad truncate size")
	}
	m.buf.Truncate(int(size))
	if m.pos > size {
		m.pos = size
	}
	return nil
}

func (m *memBuffer) Bytes() []byte { return m.buf.Bytes() }

// truncatable is implemented by *os.File and memBuffer.
type truncatable interface {
	Truncate(size int64) error
}

func putU16(b []byte, v uint16) { binary.BigEndian.PutUint16(b, v) }
func putU32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }
func putU64(b []byte, v uint64) { binary.BigEndian.PutUint64(b, v) }

// boxMark records where a box started so EndBox can patch its declared size.
type boxMark struct {
	typ    string
	offset int64
}

// BeginBox writes an 8-byte box header with a placeholder size.
func BeginBox(w io.WriteSeeker, typ string) (boxMark, error) {
	off, err := w.Seek(0, io.SeekCurrent)
	if err != nil {
		return boxMark{}, err
	}
	hdr := make([]byte, 8)
	copy(hdr[4:], typ)
	if _, err := w.Write(hdr); err != nil {
		return boxMark{}, err
	}
	return boxMark{typ: typ, offset: off}, nil
}

// EndBox patches the box size from its current end offset.
func EndBox(w io.WriteSeeker, m boxMark) error {
	end, err := w.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if _, err := w.Seek(m.offset, io.SeekStart); err != nil {
		return err
	}
	hdr := make([]byte, 8)
	putU32(hdr, uint32(end-m.offset))
	copy(hdr[4:], m.typ)
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err = w.Seek(end, io.SeekStart)
	return err
}

// BeginFullBox writes a box header plus version and flags fields.
func BeginFullBox(w io.WriteSeeker, typ string, version uint8, flags uint32) (boxMark, error) {
	m, err := BeginBox(w, typ)
	if err != nil {
		return m, err
	}
	hdr := make([]byte, 4)
	hdr[0] = version
	putU32(hdr, flags&0x00FFFFFF) // low 24 bits; byte 0 is version
	hdr[0] = version
	if _, err := w.Write(hdr); err != nil {
		return boxMark{}, err
	}
	return m, nil
}

// WriteFixed32 writes a 16.16 fixed-point number.
func WriteFixed32(w io.Writer, v float64) error {
	return binary.Write(w, binary.BigEndian, uint32(v*65536))
}
