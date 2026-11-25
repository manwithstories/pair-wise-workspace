package ebml

import (
	"encoding/binary"
	"fmt"
	"hash"
	"io"
	"math"
)

// Writer emits EBML elements. It tracks its own offset and optionally feeds a
// hash so callers can digest exactly the bytes written.
type Writer struct {
	w      io.Writer
	off    int64
	hasher hash.Hash
}

// NewWriter returns a Writer that counts bytes and updates hasher when it is
// non-nil.
func NewWriter(w io.Writer, hasher hash.Hash) *Writer {
	return &Writer{w: w, hasher: hasher}
}

// Offset returns the number of bytes written so far.
func (w *Writer) Offset() int64 { return w.off }

func (w *Writer) write(p []byte) error {
	n, err := w.w.Write(p)
	w.off += int64(n)
	if w.hasher != nil && n > 0 {
		w.hasher.Write(p[:n])
	}
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}

// EncodeID returns the canonical on-disk bytes of an element ID.
func EncodeID(id ID) []byte {
	switch {
	case id <= 0xFF:
		return []byte{byte(id)}
	case id <= 0xFFFF:
		return []byte{byte(id >> 8), byte(id)}
	case id <= 0xFFFFFF:
		return []byte{byte(id >> 16), byte(id >> 8), byte(id)}
	default:
		return []byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
	}
}

// EncodeSize returns an EBML size VINT using the fewest bytes that fit value.
// Values that need 8 bytes are encoded with an 8-byte VINT.
func EncodeSize(value int64) ([]byte, error) {
	if value < 0 {
		return nil, fmt.Errorf("ebml: negative size %d", value)
	}
	for length := 1; length <= 8; length++ {
		max := int64(1)<<(7*uint(length)) - 1
		if value < max { // all-ones means unknown, so must be strictly less
			return encodeSizeWidth(value, length), nil
		}
	}
	return nil, fmt.Errorf("ebml: size %d does not fit in 8 bytes", value)
}

// UnknownSizeBytes returns the canonical single-byte unknown-size marker.
func UnknownSizeBytes() []byte { return []byte{0xFF} }

func encodeSizeWidth(value int64, length int) []byte {
	buf := make([]byte, length)
	for i := length - 1; i >= 0; i-- {
		buf[i] = byte(value)
		value >>= 8
	}
	buf[0] |= 1 << (7 - uint(length-1))
	return buf
}

// WriteID writes an element ID.
func (w *Writer) WriteID(id ID) error { return w.write(EncodeID(id)) }

// WriteSize writes a size VINT of minimal width.
func (w *Writer) WriteSize(value int64) error {
	b, err := EncodeSize(value)
	if err != nil {
		return err
	}
	return w.write(b)
}

// WriteUnknownSize writes the unknown-size marker for a container.
func (w *Writer) WriteUnknownSize() error { return w.write(UnknownSizeBytes()) }

// Element writes a complete element with a known payload.
func (w *Writer) Element(id ID, payload []byte) error {
	if err := w.WriteID(id); err != nil {
		return err
	}
	if err := w.WriteSize(int64(len(payload))); err != nil {
		return err
	}
	return w.write(payload)
}

// StartContainer writes a container header with unknown size, for streaming
// writers that cannot know the size up front.
func (w *Writer) StartContainerUnknown(id ID) error {
	if err := w.WriteID(id); err != nil {
		return err
	}
	return w.WriteUnknownSize()
}

// Uint writes an unsigned integer element using the minimal big-endian width
// (at least one byte).
func (w *Writer) Uint(id ID, v uint64) error {
	return w.Element(id, minimalUint(v))
}

// Int writes a signed integer element using the minimal two's-complement width.
func (w *Writer) Int(id ID, v int64) error {
	return w.Element(id, minimalInt(v))
}

// String writes a UTF-8 string element.
func (w *Writer) String(id ID, s string) error { return w.Element(id, []byte(s)) }

// Float writes a float element. Matroska uses 8-byte floats for Duration and
// 4-byte for SamplingFrequency; width must be 4 or 8.
func (w *Writer) Float(id ID, v float64, width int) error {
	switch width {
	case 4:
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], math.Float32bits(float32(v)))
		return w.Element(id, b[:])
	case 8:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], math.Float64bits(v))
		return w.Element(id, b[:])
	default:
		return fmt.Errorf("ebml: invalid float width %d", width)
	}
}

// Bytes writes a binary element.
func (w *Writer) Bytes(id ID, b []byte) error { return w.Element(id, b) }

// CRC32Element writes an EBML CRC-32 element carrying value. The caller must
// have computed the CRC over the bytes that follow this element.
func (w *Writer) CRC32Element(value uint32) error {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], value)
	return w.Element(IDCRC32, b[:])
}

// WriteRaw appends bytes without interpreting them as an element. Callers use
// it to stream payloads after StartElement or StartContainerUnknown.
func (w *Writer) WriteRaw(p []byte) error { return w.write(p) }

// StartElement writes a header for an element whose size is already known, so
// the caller can stream the payload without buffering it.
func (w *Writer) StartElement(id ID, size int64) error {
	if err := w.WriteID(id); err != nil {
		return err
	}
	return w.WriteSize(size)
}

func minimalUint(v uint64) []byte {
	if v == 0 {
		return []byte{0}
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	i := 0
	for i < 7 && b[i] == 0 {
		i++
	}
	return b[i:]
}

func minimalInt(v int64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v))
	i := 0
	if v >= 0 {
		for i < 7 && b[i] == 0 && b[i+1]&0x80 == 0 {
			i++
		}
	} else {
		for i < 7 && b[i] == 0xFF && b[i+1]&0x80 != 0 {
			i++
		}
	}
	return b[i:]
}

// EncodeUint exposes minimal-width unsigned encoding for fixture builders.
func EncodeUint(v uint64) []byte { return minimalUint(v) }

// EncodeInt exposes minimal-width signed encoding for fixture builders.
func EncodeInt(v int64) []byte { return minimalInt(v) }

// EncodeFloat encodes a float of width 4 or 8.
func EncodeFloat(v float64, width int) ([]byte, error) {
	switch width {
	case 4:
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, math.Float32bits(float32(v)))
		return b, nil
	case 8:
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, math.Float64bits(v))
		return b, nil
	default:
		return nil, fmt.Errorf("ebml: invalid float width %d", width)
	}
}
