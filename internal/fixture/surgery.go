package fixture

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"
)

// SegmentSpans returns the (moof, mdat) byte spans of every fragment in data,
// in file order. It reuses the same header validation as the production parser
// so the fixture and the reader never disagree about a box's extent.
type SegmentSpan struct {
	Sequence   uint32
	Start      int64 // moof offset
	End        int64 // exclusive end of mdat
	MoofOffset int64
	MdatOffset int64
	MdatSize   int64
}

// scanSpans walks top-level boxes and records each moof/mdat adjacency.
func scanSpans(data []byte) []SegmentSpan {
	var out []SegmentSpan
	pos := int64(0)
	for pos+8 <= int64(len(data)) {
		size := int64(binary.BigEndian.Uint32(data[pos : pos+4]))
		typ := string(data[pos+4 : pos+8])
		hdr := int64(8)
		if size == 1 {
			if pos+16 > int64(len(data)) {
				break
			}
			size = int64(binary.BigEndian.Uint64(data[pos+8 : pos+16]))
			hdr = 16
		} else if size == 0 {
			size = int64(len(data)) - pos
		}
		if size < hdr || pos+size > int64(len(data)) {
			break
		}
		if typ == "moof" {
			seq := moofSequence(data[pos+int64(8) : pos+size])
			sp := SegmentSpan{Sequence: seq, Start: pos, MoofOffset: pos, End: pos + size, MdatOffset: -1}
			next := pos + size
			if next+8 <= int64(len(data)) {
				msize := int64(binary.BigEndian.Uint32(data[next : next+4]))
				if string(data[next+4:next+8]) == "mdat" {
					mhdr := int64(8)
					if msize == 1 {
						msize = int64(binary.BigEndian.Uint64(data[next+8 : next+16]))
						mhdr = 16
					} else if msize == 0 {
						msize = int64(len(data)) - next
					}
					if msize <= int64(len(data))-next {
						sp.MdatOffset = next
						sp.MdatSize = msize
						sp.End = next + msize
						_ = mhdr
					}
				}
			}
			out = append(out, sp)
			pos = sp.End
			continue
		}
		pos += size
	}
	return out
}

// moofSequence extracts mfhd sequence_number from a moof payload. It tolerates
// a malformed mfhd by returning 0 so scanning still completes.
func moofSequence(moof []byte) uint32 {
	pos := int64(0)
	for pos+8 <= int64(len(moof)) {
		size := int64(binary.BigEndian.Uint32(moof[pos : pos+4]))
		if size < 8 || pos+size > int64(len(moof)) {
			return 0
		}
		if string(moof[pos+4:pos+8]) == "mfhd" && size >= 16 {
			return binary.BigEndian.Uint32(moof[pos+12 : pos+16])
		}
		pos += size
	}
	return 0
}

// findBox returns the offset/size of the first box with the given type at the
// top level of the provided region.
func findBox(data []byte, base int64, typ string) (int64, int64, bool) {
	pos := base
	for pos+8 <= int64(len(data)) {
		size := int64(binary.BigEndian.Uint32(data[pos : pos+4]))
		hdr := int64(8)
		if size == 1 {
			if pos+16 > int64(len(data)) {
				return 0, 0, false
			}
			size = int64(binary.BigEndian.Uint64(data[pos+8 : pos+16]))
			hdr = 16
		} else if size == 0 {
			size = int64(len(data)) - pos
		}
		if size < hdr {
			return 0, 0, false
		}
		if string(data[pos+4:pos+8]) == typ {
			return pos, size, true
		}
		if pos+size > int64(len(data)) {
			return 0, 0, false
		}
		pos += size
	}
	return 0, 0, false
}

// RewriteTFDT adds delta (in track ticks, possibly negative) to the tfdt
// baseDecodeTime of every traf in the listed segment spans. It rewrites the
// field in place using its existing version width, so a v0 field stays v0.
func RewriteTFDT(data []byte, spans []SegmentSpan, delta int64) error {
	for _, sp := range spans {
		moof := data[sp.MoofOffset : sp.MoofOffset+sizeOf(data, sp.MoofOffset)]
		if err := walkMoofChildren(moof, func(typ string, rel int) error {
			if typ != "traf" {
				return nil
			}
			traf := moof[rel:]
			return walkChildren(traf, func(t string, off int, sz int) error {
				if t != "tfdt" {
					return nil
				}
				if sz < 8 {
					return fmt.Errorf("tfdt too short")
				}
				version := traf[off+8]
				field := off + 12
				if version == 1 {
					old := int64(binary.BigEndian.Uint64(traf[field : field+8]))
					next := old + delta
					if next < 0 {
						return fmt.Errorf("tfdt v1 rewrite negative: %d -> %d", old, next)
					}
					binary.BigEndian.PutUint64(traf[field:field+8], uint64(next))
					return nil
				}
				old := int64(binary.BigEndian.Uint32(traf[field : field+4]))
				next := old + delta
				if next < 0 || next >= 1<<32 {
					return fmt.Errorf("tfdt v0 rewrite out of range: %d -> %d", old, next)
				}
				binary.BigEndian.PutUint32(traf[field:field+4], uint32(next))
				return nil
			})
		}); err != nil {
			return err
		}
	}
	return nil
}

func sizeOf(data []byte, off int64) int64 {
	size := int64(binary.BigEndian.Uint32(data[off : off+4]))
	if size == 1 {
		return int64(binary.BigEndian.Uint64(data[off+8 : off+16]))
	}
	return size
}

// walkMoofChildren visits a moof's direct children, reporting each child's
// offset relative to the moof start.
func walkMoofChildren(moof []byte, fn func(typ string, rel int) error) error {
	return walkRegion(moof, 8, func(typ string, abs int, sz int) error { return fn(typ, abs) })
}

// walkChildren visits boxes starting at the slice's own box header.
func walkChildren(container []byte, fn func(typ string, off int, size int) error) error {
	// The container slice starts with a header; its payload begins at +8.
	return walkRegion(container, 8, fn)
}

func walkRegion(b []byte, start int, fn func(string, int, int) error) error {
	pos := start
	for pos+8 <= len(b) {
		size := int(binary.BigEndian.Uint32(b[pos : pos+4]))
		hdr := 8
		if size == 1 {
			if pos+16 > len(b) {
				return fmt.Errorf("largesize truncated at %d", pos)
			}
			size = int(binary.BigEndian.Uint64(b[pos+8 : pos+16]))
			hdr = 16
		} else if size == 0 {
			size = len(b) - pos
		}
		if size < hdr || pos+size > len(b) {
			return fmt.Errorf("box at %d size %d overruns", pos, size)
		}
		if err := fn(string(b[pos+4:pos+8]), pos, size); err != nil {
			return err
		}
		pos += size
	}
	return nil
}

// ReverseSegmentSwaps turns the list of spans into a new file image where the
// two listed spans exchange positions. Payload byte ranges move with their
// moof, so sample offsets stay internally consistent; the mfhd sequence order
// becomes non-monotonic, which the recovery stage must flag.
func ReverseSegmentSwaps(data []byte, spans []SegmentSpan, i, j int) ([]byte, error) {
	if i < 0 || j < 0 || i >= len(spans) || j >= len(spans) || i == j {
		return nil, fmt.Errorf("swap indices out of range")
	}
	a, b := spans[i], spans[j]
	if b.Start < a.End {
		return nil, fmt.Errorf("spans overlap")
	}
	// Build: [0:a.Start) + B + [a.End:b.Start) + A + [b.End:len)
	var out []byte
	out = append(out, data[:a.Start]...)
	out = append(out, data[b.Start:b.End]...)
	out = append(out, data[a.End:b.Start]...)
	out = append(out, data[a.Start:a.End]...)
	out = append(out, data[b.End:]...)
	return out, nil
}

// AppendDuplicate appends a byte-identical copy of a span at end of file with
// a rewritten mfhd sequence equal to the last sequence plus one. The duplicate
// payload is byte-identical, which is how the checker detects a repeated
// fragment.
func AppendDuplicate(data []byte, span SegmentSpan, newSequence uint32) []byte {
	out := make([]byte, 0, len(data)+int(span.End-span.Start))
	out = append(out, data...)
	dup := make([]byte, span.End-span.Start)
	copy(dup, data[span.Start:span.End])
	// Rewrite the mfhd sequence in the copy.
	if p, size, ok := findBox(dup, 0, "moof"); ok {
		_ = size
		_ = walkMoofChildren(dup[p:], func(typ string, rel int) error {
			if typ == "mfhd" && rel+16 <= len(dup) {
				binary.BigEndian.PutUint32(dup[rel+12:rel+16], newSequence)
			}
			return nil
		})
	}
	out = append(out, dup...)
	return out
}

// DropSpans removes the given spans from the file image.
func DropSpans(data []byte, spans []SegmentSpan, drop []int) ([]byte, error) {
	toDrop := map[int]bool{}
	for _, i := range drop {
		if i < 0 || i >= len(spans) {
			return nil, fmt.Errorf("drop index %d out of range", i)
		}
		if toDrop[i] {
			return nil, fmt.Errorf("duplicate drop index %d", i)
		}
		toDrop[i] = true
	}
	ordered := make([]int, 0, len(drop))
	for i := range drop {
		ordered = append(ordered, drop[i])
	}
	sort.Ints(ordered)
	var out []byte
	prev := int64(0)
	for _, idx := range ordered {
		sp := spans[idx]
		if sp.Start < prev {
			return nil, fmt.Errorf("spans out of order")
		}
		out = append(out, data[prev:sp.Start]...)
		prev = sp.End
	}
	out = append(out, data[prev:]...)
	return out, nil
}

// TruncateMdat cuts the last mdat so its declared size exceeds the bytes that
// remain. The moof is untouched, so the defect is exactly a declared-vs-actual
// length mismatch: the tail payload bytes are missing.
func TruncateMdat(data []byte, span SegmentSpan) ([]byte, int64, error) {
	if span.MdatOffset < 0 || span.MdatSize <= 0 {
		return nil, 0, fmt.Errorf("span has no mdat")
	}
	cut := span.MdatSize / 2
	out := make([]byte, 0, len(data)-int(cut))
	out = append(out, data[:span.MdatOffset+span.MdatSize-cut]...)
	out = append(out, data[span.End:]...)
	firstBad := int64(len(out))
	// The declared size field still claims the original length.
	return out, firstBad, nil
}

// SegmentBytes returns the raw bytes of a span.
func SegmentBytes(data []byte, sp SegmentSpan) []byte {
	return data[sp.Start:sp.End]
}

// WriteFile writes data atomically into dir via a temp file.
func WriteFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
