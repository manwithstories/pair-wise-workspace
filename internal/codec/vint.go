// Package codec holds the small deterministic Matroska/WebM binary
// primitives shared by the fixture generator and the stream parser:
// variable-length integers, sized/unknown-size elements and block headers.
// Everything here operates on byte slices; no I/O is performed.
package codec

import "errors"

// MaxVIntWidth is the maximum legal EBML VINT width in bytes.
const MaxVIntWidth = 8

// EncodeVInt encodes v with the shortest width that can hold it. It
// returns an error when v needs more than eight VINT bytes.
func EncodeVInt(v uint64) ([]byte, error) {
	for width := 1; width <= MaxVIntWidth; width++ {
		maxValue := (uint64(1) << uint(7*width)) - 1
		if v < maxValue {
			return encodeWidth(v, width), nil
		}
	}
	return nil, errors.New("codec: value exceeds 8-byte VINT range")
}

// EncodeVIntWidth forces a specific width, returning an error if v does
// not fit (the reserved all-ones value stays reserved to callers).
func EncodeVIntWidth(v uint64, width int) ([]byte, error) {
	if width < 1 || width > MaxVIntWidth {
		return nil, errors.New("codec: illegal VINT width")
	}
	if v >= (uint64(1)<<uint(7*width))-1 {
		return nil, errors.New("codec: value too wide for requested VINT width")
	}
	return encodeWidth(v, width), nil
}

func encodeWidth(v uint64, width int) []byte {
	out := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		out[i] = byte(v & 0xFF)
		v >>= 8
	}
	out[0] |= 1 << uint(8-width)
	return out
}

// UnknownSizeVInt returns the reserved unknown-data-size marker of width.
// Per RFC 9559 an element carrying it must be its parent's last child.
func UnknownSizeVInt(width int) ([]byte, error) {
	if width < 1 || width > MaxVIntWidth {
		return nil, errors.New("codec: illegal VINT width")
	}
	out := make([]byte, width)
	for i := range out {
		out[i] = 0xFF
	}
	return out, nil
}

// EncodeElement wraps payload in id + minimal size VINT.
func EncodeElement(id uint32, payload []byte) ([]byte, error) {
	size, err := EncodeVInt(uint64(len(payload)))
	if err != nil {
		return nil, err
	}
	idBytes, err := EncodeID(id)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(idBytes)+len(size)+len(payload))
	out = append(out, idBytes...)
	out = append(out, size...)
	return append(out, payload...), nil
}

// EncodeID encodes a Matroska element ID; its numeric form keeps the
// length-marker bits (e.g. Segment 0x18538067).
func EncodeID(id uint32) ([]byte, error) {
	for _, width := range []int{1, 2, 3, 4} {
		if uint64(id) < uint64(1)<<uint(7*width+1) {
			return encodeWidth(uint64(id), width), nil
		}
	}
	return nil, errors.New("codec: element id wider than four VINT bytes")
}

// DecodeVInt decodes one size-style VINT from the front of b. It returns
// the value, the consumed byte count, and ok=false when the value is the
// reserved unknown marker.
func DecodeVInt(b []byte) (value uint64, n int, ok bool, err error) {
	if len(b) == 0 {
		return 0, 0, false, errors.New("codec: empty VINT buffer")
	}
	lead := b[0]
	if lead == 0 {
		return 0, 0, false, errors.New("codec: illegal VINT lead byte 0x00")
	}
	width := 1
	for mask := byte(0x80); lead&mask == 0; mask >>= 1 {
		width++
	}
	if width > len(b) {
		return 0, 0, false, errors.New("codec: truncated VINT")
	}
	v := uint64(lead & (0xFF >> uint(width)))
	for i := 1; i < width; i++ {
		v = (v << 8) | uint64(b[i])
	}
	marker := (uint64(1) << uint(7*width)) - 1
	if v == marker {
		return 0, width, false, nil
	}
	return v, width, true, nil
}

// DecodeID decodes an element ID VINT keeping marker bits in the value.
func DecodeID(b []byte) (id uint32, n int, err error) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, errors.New("codec: illegal element id")
	}
	width := 1
	for mask := byte(0x80); b[0]&mask == 0; mask >>= 1 {
		width++
	}
	if width > 4 || width > len(b) {
		return 0, 0, errors.New("codec: element id out of range or truncated")
	}
	var v uint64
	for i := 0; i < width; i++ {
		v = (v << 8) | uint64(b[i])
	}
	return uint32(v), width, nil
}

// EncodeBlockHeader builds a Matroska Block/SimpleBlock header: track VINT
// (1 byte in our fixture range), signed 16-bit relative timecode, flags.
func EncodeBlockHeader(track uint8, relativeTimecode int16, flags byte) []byte {
	return []byte{0x80 | track, byte(relativeTimecode >> 8), byte(relativeTimecode), flags}
}

// DecodeBlockHeader parses the four-byte block header emitted above. The
// returned track number strips the VINT marker (we only emit one-byte IDs).
func DecodeBlockHeader(b []byte) (track uint8, relative int16, flags byte, err error) {
	if len(b) < 4 {
		return 0, 0, 0, errors.New("codec: truncated block header")
	}
	if b[0]&0x80 == 0 {
		return 0, 0, 0, errors.New("codec: block track VINT must be one byte")
	}
	track = b[0] & 0x7F
	relative = int16(uint16(b[1])<<8 | uint16(b[2]))
	return track, relative, b[3], nil
}
