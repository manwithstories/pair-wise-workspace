// Package ebml implements a streaming, memory-bounded EBML parser for
// Matroska/WebM. The parser never buffers a whole element body: it
// reports element headers with absolute byte offsets and lets callers
// read or skip bodies incrementally. Unknown-length elements
// (size all-ones, as used by live Matroska Segments/Clusters) are
// tracked as open scopes closed only by their parent's end or EOF.
package ebml

import (
	"io"

	"mediapack/internal/clock"
)

// MaxVINTLen is the EBML maximum VINT width (Element IDs use <=4 bytes,
// size fields use <=8).
const MaxVINTLen = 8

// ReadVINT decodes one EBML variable-length integer from r. It returns
// the value, the number of bytes consumed and the raw bytes (so callers
// can preserve on-wire encodings). keepMarker controls whether the
// length-marker bit stays in the returned value (needed for Element IDs,
// stripped for sizes).
func ReadVINT(r io.Reader, keepMarker bool) (value uint64, width int, raw []byte, err error) {
	var head [1]byte
	if _, err = io.ReadFull(r, head[:]); err != nil {
		return 0, 0, nil, err
	}
	b0 := head[0]
	if b0 == 0 {
		return 0, 0, nil, clock.NewError(clock.CatBadVINT, "VINT leading byte is zero")
	}
	width = 1
	for mask := byte(0x80); mask&b0 == 0; mask >>= 1 {
		width++
		if width > MaxVINTLen {
			return 0, 0, nil, clock.NewError(clock.CatBadVINT, "VINT wider than 8 bytes")
		}
	}
	raw = make([]byte, width)
	raw[0] = b0
	if width > 1 {
		if _, err = io.ReadFull(r, raw[1:]); err != nil {
			return 0, 0, raw, err
		}
	}
	value = uint64(b0)
	if !keepMarker {
		value &= ^(uint64(1) << (8 - uint(width))) // clear marker bit
	}
	for i := 1; i < width; i++ {
		value = (value << 8) | uint64(raw[i])
	}
	return value, width, raw, nil
}

// UnknownSize is the sentinel returned when a size VINT encodes the
// all-ones "unknown length" form.
func UnknownSize(width int) uint64 {
	return (uint64(1) << uint(7*width)) - 1
}

// IsUnknownSize reports whether a decoded size of the given width is the
// EBML unknown-length marker.
func IsUnknownSize(size uint64, width int) bool {
	return width > 0 && size == UnknownSize(width)
}

// EncodeVINT encodes value as a size VINT (marker stripped form) using
// the smallest possible width. The EBML all-ones form is reserved for
// unknown length, so a value equal to a width's maximum is promoted to
// the next wider encoding. Returns nil only if value exceeds 56 payload
// bits (the 8-byte VINT capacity).
func EncodeVINT(value uint64) []byte {
	for width := 1; width <= MaxVINTLen; width++ {
		maxVal := (uint64(1) << uint(7*width)) - 1
		if width == MaxVINTLen {
			maxVal = (uint64(1) << 56) - 2 // reserve all-ones for unknown
		}
		if value <= maxVal {
			return encodeAtWidth(value, width)
		}
	}
	return nil
}

// EncodeUnknownSize renders the unknown-length marker at the requested
// VINT width (1..8).
func EncodeUnknownSize(width int) []byte {
	if width < 1 || width > MaxVINTLen {
		return nil
	}
	return encodeAtWidth(UnknownSize(width), width)
}

func encodeAtWidth(value uint64, width int) []byte {
	out := make([]byte, width)
	v := value
	for i := width - 1; i >= 0; i-- {
		out[i] = byte(v & 0xff)
		v >>= 8
	}
	out[0] |= byte(1) << uint(8-width) // set length marker
	return out
}

// EncodeElementID renders an Element ID exactly as stored on the wire
// (the marker bit is part of the ID).
func EncodeElementID(id uint64) []byte {
	for width := 1; width <= 4; width++ {
		if id < uint64(1)<<uint(8*width) {
			out := make([]byte, width)
			v := id
			for i := width - 1; i >= 0; i-- {
				out[i] = byte(v & 0xff)
				v >>= 8
			}
			return out
		}
	}
	return nil
}
