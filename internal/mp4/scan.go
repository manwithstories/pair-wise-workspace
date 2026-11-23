package mp4

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Box is one parsed box header with its position in the source byte stream.
// Payload is not retained; children hold the nested structure for containers.
type Box struct {
	Type    BoxType
	Header  int64 // offset of the size field
	Payload int64 // offset where the box payload begins
	Size    int64 // total declared box size including header
	Depth   int
	Parent  *Box
	Child   []*Box
}

// End returns the exclusive end offset of the box.
func (b *Box) End() int64 { return b.Header + b.Size }

// IsContainer reports whether the box type may hold nested boxes.
func (b *Box) IsContainer() bool {
	switch b.Type {
	case BoxMoov, BoxTrak, BoxMdia, BoxMinf, BoxStbl:
		return true
	default:
		return false
	}
}

// ScanBoxes walks the box hierarchy of data. It records every box header,
// enforces depth/count/size limits and returns ErrTruncated (wrapped with the
// offending offset) as soon as a declared box runs past the available bytes.
func ScanBoxes(data []byte, lim Limits) ([]*Box, error) {
	if err := lim.Validate(); err != nil {
		return nil, err
	}
	if int64(len(data)) > lim.MaxFileBytes {
		return nil, fmt.Errorf("%w: file size %d exceeds limit %d",
			ErrLimit, len(data), lim.MaxFileBytes)
	}
	roots := make([]*Box, 0, 8)
	counter := 0
	var walk func(start, end int64, depth int, parent *Box) ([]*Box, error)
	walk = func(start, end int64, depth int, parent *Box) ([]*Box, error) {
		if depth > lim.MaxBoxDepth {
			return nil, fmt.Errorf("%w: depth %d at offset %d",
				ErrLimit, depth, start)
		}
		var out []*Box
		pos := start
		for pos < end {
			if end-pos < 8 {
				return nil, fmt.Errorf("%w: incomplete box header at offset %d",
					ErrTruncated, pos)
			}
			size32 := int64(binary.BigEndian.Uint32(data[pos : pos+4]))
			var btype BoxType
			copy(btype[:], data[pos+4:pos+8])
			headerLen := int64(8)
			size := size32
			switch size32 {
			case 0: // box extends to end of file
				size = end - pos
			case 1: // 64-bit largesize follows the type
				if end-pos < 16 {
					return nil, fmt.Errorf("%w: missing largesize at offset %d",
						ErrTruncated, pos)
				}
				size = int64(binary.BigEndian.Uint64(data[pos+8 : pos+16]))
				headerLen = 16
			}
			if size < headerLen {
				return out, fmt.Errorf("%w: declared size %d smaller than header at offset %d",
					ErrTruncated, size, pos)
			}
			box := &Box{
				Type: btype, Header: pos,
				Payload: pos + headerLen, Size: size,
				Depth: depth, Parent: parent,
			}
			// Retain the offending box before failing so callers can still
			// localize where truncation began.
			out = append(out, box)
			if pos+size > end {
				return out, fmt.Errorf("%w: box %s at offset %d declares size %d, only %d bytes remain",
					ErrTruncated, btype, pos, size, end-pos)
			}
			counter++
			if counter > lim.MaxBoxes {
				return out, fmt.Errorf("%w: more than %d boxes", ErrLimit, lim.MaxBoxes)
			}
			if box.IsContainer() {
				kids, err := walk(box.Payload, box.End(), depth+1, box)
				if err != nil {
					return out, err
				}
				box.Child = kids
			}
			pos += size
		}
		return out, nil
	}
	var err error
	roots, err = walk(0, int64(len(data)), 0, nil)
	if err != nil {
		return roots, err
	}
	return roots, nil
}

// FindAll returns boxes of the given type anywhere in the hierarchy, in
// depth-first scan order (which is also file-offset order).
func FindAll(roots []*Box, t BoxType) []*Box {
	var out []*Box
	var rec func(boxes []*Box)
	rec = func(boxes []*Box) {
		for _, b := range boxes {
			if b.Type == t {
				out = append(out, b)
			}
			rec(b.Child)
		}
	}
	rec(roots)
	return out
}

// First returns the first box of the type or nil.
func First(roots []*Box, t BoxType) *Box {
	got := FindAll(roots, t)
	if len(got) == 0 {
		return nil
	}
	return got[0]
}

// AsTruncated reports whether err is a truncation finding and returns it.
func AsTruncated(err error) bool {
	return errors.Is(err, ErrTruncated)
}
