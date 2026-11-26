package isobmff

import (
	"encoding/binary"
	"math"

	"github.com/pairwise/relay/internal/media"
)

// Parser limits keep malformed input from driving unbounded work.
const (
	MaxInputBytes    = 64 << 20
	MaxBoxesPerLevel = 4096
)

// Box is a bounds-checked ISO-BMFF box. Payload is a sub-slice of the input
// and is valid only for the declared/available bounds accepted by ParseBoxes.
type Box struct {
	Type          string
	Offset        int64
	HeaderSize    int64
	TotalSize     int64
	PayloadOffset int64
	Payload       []byte
}

// End returns the declared end offset.
func (b Box) End() int64 { return b.Offset + b.TotalSize }

// ReadHeader reads and validates one box header at offset off.
//
// Truncation faults use the first unavailable byte (the supplied end);
// impossible size fields point at the start of the damaged length field.
func ReadHeader(data []byte, off, end int64, id media.FragmentID) (Box, *media.Fault) {
	if off < 0 || end < off || end > int64(len(data)) {
		return Box{}, media.NewFault(media.CodeBoxLength, id, off, false, "invalid reader bounds off=%d end=%d", off, end)
	}
	if off == end {
		return Box{}, nil
	}
	if end-off < 8 {
		return Box{}, media.NewFault(media.CodeBoxTruncated, id, end, true, "box header needs 8 bytes, has %d", end-off)
	}

	size32 := binary.BigEndian.Uint32(data[off : off+4])
	boxType := string(data[off+4 : off+8])
	h := Box{Type: boxType, Offset: off, HeaderSize: 8}
	next := off + 8

	switch size32 {
	case 0:
		h.TotalSize = end - off
	case 1:
		if end-off < 16 {
			return Box{}, media.NewFault(media.CodeBoxTruncated, id, end, true, "extended box header needs 16 bytes, has %d", end-off)
		}
		ext := binary.BigEndian.Uint64(data[off+8 : off+16])
		h.HeaderSize = 16
		if ext < 16 {
			return Box{}, media.NewFault(media.CodeBoxLength, id, off, false, "extended box size %d is shorter than 16-byte header", ext)
		}
		if ext > uint64(math.MaxInt64) {
			return Box{}, media.NewFault(media.CodeIntegerOverflow, id, off, false, "extended box size %d overflows signed offset", ext)
		}
		h.TotalSize = int64(ext)
		next = off + 16
	default:
		if size32 < 8 {
			return Box{}, media.NewFault(media.CodeBoxLength, id, off, false, "32-bit box size %d is shorter than 8-byte header", size32)
		}
		h.TotalSize = int64(size32)
	}

	if h.TotalSize < h.HeaderSize {
		return Box{}, media.NewFault(media.CodeBoxLength, id, off, false, "box size %d < header %d", h.TotalSize, h.HeaderSize)
	}
	declaredEnd, overflow := safeAdd(off, h.TotalSize)
	if overflow || declaredEnd > math.MaxInt64 {
		return Box{}, media.NewFault(media.CodeIntegerOverflow, id, off, false, "box end offset overflows: start=%d size=%d", off, h.TotalSize)
	}
	if declaredEnd > end {
		// The declared payload passes the available parent/input boundary.
		// end is the first byte not available in this bounded parse.
		return Box{}, media.NewFault(media.CodeBoxTruncated, id, end, true, "box %q declares end=%d beyond available end=%d", boxType, declaredEnd, end)
	}

	h.PayloadOffset = next
	h.Payload = data[next:declaredEnd:declaredEnd]
	return h, nil
}

// ParseBoxes parses sibling boxes contained in data[start:end].
func ParseBoxes(data []byte, start, end int64, id media.FragmentID) ([]Box, *media.Fault) {
	if len(data) > MaxInputBytes {
		return nil, media.NewFault(media.CodeInputTooLarge, id, 0, false, "input is %d bytes, limit %d", len(data), MaxInputBytes)
	}
	if start < 0 || end < start || end > int64(len(data)) {
		return nil, media.NewFault(media.CodeBoxLength, id, start, false, "invalid parse bounds start=%d end=%d len=%d", start, end, len(data))
	}
	boxes := make([]Box, 0, 8)
	off := start
	for off < end {
		if len(boxes) >= MaxBoxesPerLevel {
			return boxes, media.NewFault(media.CodeBoxLength, id, off, false, "more than %d boxes at one level", MaxBoxesPerLevel)
		}
		b, f := ReadHeader(data, off, end, id)
		if f != nil {
			return boxes, f
		}
		if b.Type == "" {
			break
		}
		boxes = append(boxes, b)
		off = b.End()
	}
	return boxes, nil
}

// ParseTopBoxes parses the complete input as top-level ISO-BMFF boxes.
func ParseTopBoxes(data []byte, id media.FragmentID) ([]Box, *media.Fault) {
	if len(data) == 0 {
		return nil, media.NewFault(media.CodeBoxTruncated, id, 0, true, "empty fragment")
	}
	return ParseBoxes(data, 0, int64(len(data)), id)
}

// FindChild returns the first direct child of the requested type.
func FindChild(data []byte, parent Box, typ string, id media.FragmentID) (Box, bool, *media.Fault) {
	children, f := ParseBoxes(data, parent.PayloadOffset, parent.End(), id)
	if f != nil {
		return Box{}, false, f
	}
	for _, c := range children {
		if c.Type == typ {
			return c, true, nil
		}
	}
	return Box{}, false, nil
}

// FindChildren returns all direct children of the requested type.
func FindChildren(data []byte, parent Box, typ string, id media.FragmentID) ([]Box, *media.Fault) {
	children, f := ParseBoxes(data, parent.PayloadOffset, parent.End(), id)
	if f != nil {
		return nil, f
	}
	out := make([]Box, 0, 2)
	for _, c := range children {
		if c.Type == typ {
			out = append(out, c)
		}
	}
	return out, nil
}

func safeAdd(a, b int64) (int64, bool) {
	if b < 0 || a > math.MaxInt64-b {
		return 0, true
	}
	return a + b, false
}

// U32 reads a big-endian uint32 with an explicit bounds fault.
func U32(b []byte, id media.FragmentID, field string, baseOffset int64) (uint32, *media.Fault) {
	if len(b) < 4 {
		return 0, media.NewFault(media.CodeBoxTruncated, id, baseOffset+int64(len(b)), true, "%s needs 4 bytes, has %d", field, len(b))
	}
	return binary.BigEndian.Uint32(b[:4]), nil
}

// U64 reads a big-endian uint64 with an explicit bounds fault.
func U64(b []byte, id media.FragmentID, field string, baseOffset int64) (uint64, *media.Fault) {
	if len(b) < 8 {
		return 0, media.NewFault(media.CodeBoxTruncated, id, baseOffset+int64(len(b)), true, "%s needs 8 bytes, has %d", field, len(b))
	}
	return binary.BigEndian.Uint64(b[:8]), nil
}
