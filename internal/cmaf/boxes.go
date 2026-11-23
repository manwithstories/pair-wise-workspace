package cmaf

import (
	"encoding/binary"
	"fmt"

	"mediaflow/internal/clock"
)

// Box is one ISOBMFF box header located inside a bounded byte range. End is
// the declared end offset; Valid is false when the declared size cannot fit
// inside the containing range.
type Box struct {
	Type        string
	Offset      int
	HeaderSize  int
	Size        uint64
	End         int
	Valid       bool
	ExtendsEOF  bool
	declaredEnd int
}

// Problem is a local parse failure. It never aborts a scan: the walker always
// attempts to continue at a determinable boundary.
type Problem struct {
	Kind   clock.ErrorKind
	Offset int
	Detail string
}

func (p Problem) Error() string { return fmt.Sprintf("%s at %d: %s", p.Kind, p.Offset, p.Detail) }

// TopLevelTypes are the four-character codes that begin a CMAF segment or
// initialization segment. The resynchronizer only trusts these anchors.
var TopLevelTypes = [][4]byte{
	{'f', 't', 'y', 'p'}, {'s', 't', 'y', 'p'}, {'m', 'o', 'o', 'f'},
	{'m', 'd', 'a', 't'}, {'m', 'o', 'o', 'v'}, {'s', 'i', 'd', 'x'},
	{'e', 'm', 's', 'g'}, {'f', 'r', 'e', 'e'},
}

func isTopLevel(t string) bool {
	for _, c := range TopLevelTypes {
		if string(c[:]) == t {
			return true
		}
	}
	return false
}

// ParseBoxHeader parses one box header from buf at off, bounded by limit.
// It returns the header even when the declared size overruns limit, with
// Valid=false so the caller can record an error and still resynchronize.
func ParseBoxHeader(buf []byte, off, limit int) (Box, error) {
	if off < 0 || off+8 > limit {
		return Box{}, fmt.Errorf("cmaf: box header at %d exceeds limit %d", off, limit)
	}
	size := uint64(binary.BigEndian.Uint32(buf[off:]))
	typ := string(buf[off+4 : off+8])
	header := 8
	ext := false
	switch size {
	case 0:
		size = uint64(limit - off)
		ext = true
	case 1:
		if off+16 > limit {
			return Box{}, fmt.Errorf("cmaf: 64-bit box header at %d is truncated", off)
		}
		size = binary.BigEndian.Uint64(buf[off+8:])
		header = 16
	}
	b := Box{Type: typ, Offset: off, HeaderSize: header, Size: size, ExtendsEOF: ext}
	if size < uint64(header) {
		return b, fmt.Errorf("cmaf: box %q at %d declares size %d smaller than header", typ, off, size)
	}
	if size > uint64(limit-off) {
		b.Valid = false
		b.declaredEnd = off + int(size)
		b.End = limit
		return b, nil
	}
	b.Valid = true
	b.End = off + int(size)
	b.declaredEnd = b.End
	return b, nil
}

// ParseChildren walks the immediate children of a box payload in
// buf[payloadOff:box.End]. Problems are appended and scanning continues when a
// later boundary is recognizable.
func ParseChildren(buf []byte, parent Box, problems *[]Problem) []Box {
	if parent.HeaderSize > parent.End-parent.Offset {
		*problems = append(*problems, Problem{clock.ErrBoxTruncated, parent.Offset, "header past box end"})
		return nil
	}
	start := parent.Offset + parent.HeaderSize
	limit := parent.End
	if !parent.Valid {
		// A corrupt parent size still lets us inspect its header-adjacent
		// children; the payload limit is the buffer end.
		limit = len(buf)
	}
	var out []Box
	for off := start; off+8 <= limit; {
		b, err := ParseBoxHeader(buf, off, limit)
		if err != nil {
			*problems = append(*problems, Problem{clock.ErrBoxTruncated, off, err.Error()})
			break
		}
		if !b.Valid {
			*problems = append(*problems, Problem{clock.ErrBoxSizeOverrun, off,
				fmt.Sprintf("box %q declares end %d beyond limit %d", b.Type, b.declaredEnd, limit)})
			// The declared length is untrustworthy. Keep only the header and
			// resume at the next known sibling type, if any.
			out = append(out, b)
			next := resync(buf, off+b.HeaderSize, limit, parent)
			if next < 0 {
				break
			}
			off = next
			continue
		}
		out = append(out, b)
		off = b.End
	}
	return out
}

// resync scans forward for the next plausible box header. Children are not
// restricted to top-level types; the heuristic accepts any printable
// four-character type with a size that fits, anchored on a zero byte or a
// known sibling type. Bounded by limit to avoid unbounded work.
func resync(buf []byte, start, limit int, parent Box) int {
	for off := start; off+8 <= limit; off++ {
		if buf[off] != 0 && buf[off] != 'm' && buf[off] != 't' && buf[off] != 'e' && buf[off] != 's' && buf[off] != 'f' {
			continue
		}
		t := string(buf[off+4 : off+8])
		if !printableType(t) {
			continue
		}
		b, err := ParseBoxHeader(buf, off, limit)
		if err != nil || !b.Valid || b.Size < 8 {
			continue
		}
		return off
	}
	return -1
}

func printableType(t string) bool {
	for i := 0; i < len(t); i++ {
		if t[i] < 0x20 || t[i] > 0x7e {
			return false
		}
	}
	return true
}

// WalkTopLevel parses the sequence of top-level boxes. A box with an
// overrunning declared size is reported and the walker resynchronizes at the
// next known top-level type, so later segments remain inspectable.
func WalkTopLevel(buf []byte, maxBoxSize uint64) ([]Box, []Problem) {
	var boxes []Box
	var problems []Problem
	limit := len(buf)
	for off := 0; off+8 <= limit; {
		b, err := ParseBoxHeader(buf, off, limit)
		if err != nil {
			problems = append(problems, Problem{clock.ErrBoxTruncated, off, err.Error()})
			next := nextTopLevel(buf, off+1, limit)
			if next < 0 {
				break
			}
			problems = append(problems, Problem{clock.ErrResync, next, "resynchronized after truncated header"})
			off = next
			continue
		}
		if maxBoxSize > 0 && b.Size > maxBoxSize {
			problems = append(problems, Problem{clock.ErrBoxSizeOverrun, off,
				fmt.Sprintf("box %q size %d exceeds configured limit %d", b.Type, b.Size, maxBoxSize)})
			next := nextTopLevel(buf, off+b.HeaderSize, limit)
			if next < 0 {
				break
			}
			problems = append(problems, Problem{clock.ErrResync, next, "resynchronized after oversized box"})
			off = next
			continue
		}
		if !b.Valid {
			problems = append(problems, Problem{clock.ErrBoxSizeOverrun, off,
				fmt.Sprintf("box %q declares end %d beyond buffer %d", b.Type, b.declaredEnd, limit)})
			next := nextTopLevel(buf, off+b.HeaderSize, limit)
			if next < 0 {
				break
			}
			problems = append(problems, Problem{clock.ErrResync, next, "resynchronized after overrun box"})
			off = next
			continue
		}
		boxes = append(boxes, b)
		off = b.End
	}
	return boxes, problems
}

func nextTopLevel(buf []byte, start, limit int) int {
	for off := start; off+8 <= limit; off++ {
		t := string(buf[off+4 : off+8])
		if isTopLevel(t) {
			if b, err := ParseBoxHeader(buf, off, limit); err == nil && b.Valid {
				return off
			}
		}
	}
	return -1
}
