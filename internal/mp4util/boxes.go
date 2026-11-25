// Package mp4util provides deterministic ISO BMFF box writing shared by the
// fixture generator and the lossless remuxer. It never mutates sample bytes.
package mp4util

import (
	"bytes"
	"encoding/binary"
	"errors"

	"github.com/abema/go-mp4"
)

// ErrBoxTooLarge is returned when a box cannot use a 32-bit size field.
var ErrBoxTooLarge = errors.New("mp4util: box exceeds 32-bit size")

// BoxWriter accumulates nested boxes in memory.
type BoxWriter struct {
	buf   bytes.Buffer
	stack []int
}

// New returns an empty BoxWriter.
func New() *BoxWriter { return &BoxWriter{} }

// Len reports the number of bytes written so far.
func (w *BoxWriter) Len() int { return w.buf.Len() }

// Bytes returns the accumulated bytes. The buffer is not reset.
func (w *BoxWriter) Bytes() []byte { return w.buf.Bytes() }

// Start opens a container box, reserving an 8-byte header.
func (w *BoxWriter) Start(typ string) {
	w.stack = append(w.stack, w.buf.Len())
	var hdr [8]byte
	copy(hdr[4:], typ)
	w.buf.Write(hdr[:])
}

// End closes the innermost box and patches its size.
func (w *BoxWriter) End() error {
	if len(w.stack) == 0 {
		return errors.New("mp4util: End without Start")
	}
	start := w.stack[len(w.stack)-1]
	w.stack = w.stack[:len(w.stack)-1]
	size := w.buf.Len() - start
	if size > int(^uint32(0)) {
		return ErrBoxTooLarge
	}
	binary.BigEndian.PutUint32(w.buf.Bytes()[start:], uint32(size))
	return nil
}

// WriteRaw writes a leaf box with the given payload.
func (w *BoxWriter) WriteRaw(typ string, payload []byte) error {
	if len(payload)+8 > int(^uint32(0)) {
		return ErrBoxTooLarge
	}
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)+8))
	copy(hdr[4:], typ)
	w.buf.Write(hdr[:])
	w.buf.Write(payload)
	return nil
}

// WriteRawBytes appends already-serialized boxes verbatim.
func (w *BoxWriter) WriteRawBytes(b []byte) { w.buf.Write(b) }

// Marshal marshals a go-mp4 box and writes it with a header.
func (w *BoxWriter) Marshal(box mp4.IImmutableBox) error {
	var payload bytes.Buffer
	if _, err := mp4.Marshal(&payload, box, mp4.Context{}); err != nil {
		return err
	}
	t := box.GetType()
	return w.WriteRaw(string(t[:]), payload.Bytes())
}

// WriteFullBox writes a full box header and payload.
func (w *BoxWriter) WriteFullBox(typ string, version uint8, flags uint32, payload []byte) error {
	body := make([]byte, 4+len(payload))
	body[0] = version
	body[1] = byte(flags >> 16)
	body[2] = byte(flags >> 8)
	body[3] = byte(flags)
	copy(body[4:], payload)
	return w.WriteRaw(typ, body)
}

// WriteStyp writes a segment type box declaring the given brands.
func (w *BoxWriter) WriteStyp(major string, compatible ...string) error {
	payload := make([]byte, 8, 8+4*len(compatible))
	copy(payload[0:4], major)
	for _, c := range compatible {
		var b [4]byte
		copy(b[:], c)
		payload = append(payload, b[:]...)
	}
	return w.WriteRaw("styp", payload)
}

// WriteFtyp writes a file type box.
func (w *BoxWriter) WriteFtyp(major string, compatible ...string) error {
	return w.WriteStyp2("ftyp", major, compatible...)
}

// WriteStyp2 is the shared implementation of ftyp/styp.
func (w *BoxWriter) WriteStyp2(typ, major string, compatible ...string) error {
	payload := make([]byte, 8, 8+4*len(compatible))
	copy(payload[0:4], major)
	for _, c := range compatible {
		var b [4]byte
		copy(b[:], c)
		payload = append(payload, b[:]...)
	}
	return w.WriteRaw(typ, payload)
}

// WriteStsd writes a sample description box with pre-serialized sample entries.
func (w *BoxWriter) WriteStsd(entries [][]byte) error {
	w.Start("stsd")
	var verFlags [4]byte
	var count [4]byte
	binary.BigEndian.PutUint32(count[:], uint32(len(entries)))
	w.buf.Write(verFlags[:])
	w.buf.Write(count[:])
	for _, e := range entries {
		w.buf.Write(e)
	}
	return w.End()
}

// ReadRawBox returns the complete bytes of the box described by bi, header
// included, from r. It is used to preserve sample entries verbatim.
func ReadRawBox(r interface {
	ReadAt([]byte, int64) (int, error)
}, offset, size uint64) ([]byte, error) {
	if size > 16<<20 {
		return nil, errors.New("mp4util: refusing to copy oversized box")
	}
	buf := make([]byte, size)
	n, err := r.ReadAt(buf, int64(offset))
	if err != nil && n != int(size) {
		return nil, err
	}
	return buf, nil
}

// TruncatedBoxError reports a box whose declared size runs past the input.
type TruncatedBoxError struct {
	Offset uint64
	Size   uint64
	Limit  uint64
}

func (e *TruncatedBoxError) Error() string {
	return "mp4util: truncated box at offset " + itoa(e.Offset) + ": needs " + itoa(e.Size) + " bytes, file has " + itoa(e.Limit)
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
