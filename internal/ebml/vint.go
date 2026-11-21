// Package ebml implements a streaming EBML parser that preserves the source
// byte offset of every element and tolerates truncated tails so that every
// fully confirmed element before the damage remains recoverable.
package ebml

import (
	"io"

	"mkvarchive/internal/model"
)

// UnknownSize marks an EBML element whose size is encoded as all VINT data
// bits set to one (the "unknown length" form, e.g. live Matroska segments).
const UnknownSize int64 = -1

// MaxVINTWidth is the largest legal EBML variable-length integer width.
const MaxVINTWidth = 8

// VINT is a decoded EBML variable-length integer together with its width.
type VINT struct {
	Value uint64
	Width int
}

// markerMasks[i] has the VINT length marker bit set for an i-byte integer.
var markerMasks = [MaxVINTWidth + 1]byte{
	0, 0x80, 0x40, 0x20, 0x10, 0x08, 0x04, 0x02, 0x01,
}

// ReadVINT decodes one VINT from r. absOffset is the absolute source offset
// of the first byte and is used to anchor truncation errors precisely.
// The length marker bit is stripped from the returned value.
func ReadVINT(r io.Reader, absOffset int64) (VINT, error) {
	return readVINT(r, absOffset, false)
}

// ReadElementID decodes a VINT that carries an EBML element identifier,
// preserving the length marker bit which is part of the ID semantics.
func ReadElementID(r io.Reader, absOffset int64) (VINT, error) {
	return readVINT(r, absOffset, true)
}

func readVINT(r io.Reader, absOffset int64, keepMarker bool) (VINT, error) {
	var head [1]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		// A clean EOF at a header boundary legally terminates unknown-length
		// masters; only partial VINT bytes count as truncation.
		if err == io.EOF {
			return VINT{}, io.EOF
		}
		return VINT{}, model.NewError("EBML_TRUNCATED_HEADER",
			"truncated VINT length marker", absOffset)
	}
	width := 0
	for w := 1; w <= MaxVINTWidth; w++ {
		if head[0]&markerMasks[w] != 0 {
			width = w
			break
		}
	}
	if width == 0 {
		return VINT{}, model.NewError("EBML_BAD_VINT",
			"no VINT length marker in leading byte", absOffset)
	}
	buf := make([]byte, width)
	buf[0] = head[0]
	if width > 1 {
		if _, err := io.ReadFull(r, buf[1:]); err != nil {
			return VINT{}, model.NewError("EBML_TRUNCATED_HEADER",
				"VINT payload extends past end of input", absOffset)
		}
	}
	first := buf[0]
	if !keepMarker {
		first &^= markerMasks[width]
	}
	value := uint64(first)
	for _, b := range buf[1:] {
		value = (value << 8) | uint64(b)
	}
	return VINT{Value: value, Width: width}, nil
}

// UnknownSizeValue returns the all-ones data value that encodes an unknown
// length for a VINT of the given width.
func UnknownSizeValue(width int) uint64 {
	return uint64(1)<<uint(7*width) - 1
}

// EncodeVINTWidth encodes value with an explicit VINT width. The value must
// fit the 7*width available data bits.
func EncodeVINTWidth(value uint64, width int) ([]byte, error) {
	if width < 1 || width > MaxVINTWidth {
		return nil, model.NewError("EBML_BAD_VINT", "unsupported VINT width", -1)
	}
	if value > UnknownSizeValue(width) {
		return nil, model.NewError("EBML_BAD_VINT", "value overflows VINT width", -1)
	}
	out := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		out[i] = byte(value & 0xFF)
		value >>= 8
	}
	out[0] |= markerMasks[width]
	return out, nil
}

// EncodeVINT returns the shortest legal VINT encoding of value, preferring
// the canonical widths Matroska uses on the wire.
func EncodeVINT(value uint64) ([]byte, error) {
	for width := 1; width <= MaxVINTWidth; width++ {
		if value < UnknownSizeValue(width) {
			return EncodeVINTWidth(value, width)
		}
	}
	return nil, model.NewError("EBML_BAD_VINT", "value exceeds maximum VINT range", -1)
}

// EncodeID encodes an EBML element identifier. EBML IDs already carry their
// VINT length marker in the leading byte, so the width is fixed by the ID
// itself rather than chosen by numeric magnitude.
func EncodeID(id uint32) ([]byte, error) {
	shift := -1
	for s := 24; s >= 0; s -= 8 {
		if byte(id>>uint(s)) != 0 {
			shift = s
			break
		}
	}
	if shift < 0 {
		return nil, model.NewError("EBML_BAD_ELEMENT_ID", "zero identifier", -1)
	}
	width := shift/8 + 1
	leading := byte(id >> uint(shift))
	if leading&markerMasks[width] == 0 {
		return nil, model.NewError("EBML_BAD_ELEMENT_ID",
			"identifier leading byte lacks the VINT marker for its width", -1)
	}
	// IDs carry the marker in their numeric value, so emit the raw
	// big-endian bytes rather than the data-only EncodeVINTWidth path.
	out := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		out[i] = byte(id & 0xFF)
		id >>= 8
	}
	return out, nil
}
