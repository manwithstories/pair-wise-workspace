// Package box implements a self-contained MP4 (ISO BMFF) box tree parser.
// go-mp4 is used only as the low-level box-header codec; the tree walk,
// container handling, uuid boxes and bounds checking live here.
package box

// Node is one box in the parsed tree. Leaf boxes keep only their header
// geometry; payload bytes are read on demand via an io.ReaderAt.
type Node struct {
	Type       string    // four-character code, or "uuid"
	Offset     int64     // absolute file offset of the box header
	Size       int64     // total size in bytes, header included
	HeaderSize int64     // 8, 16, or 24/32 for uuid boxes
	UUID       *[16]byte // usertype, set only when Type == "uuid"
	Children   []*Node
}

// DataOffset returns the absolute offset of the box payload.
func (n *Node) DataOffset() int64 { return n.Offset + n.HeaderSize }

// DataSize returns the payload size in bytes.
func (n *Node) DataSize() int64 { return n.Size - n.HeaderSize }

// End returns the absolute offset one past the box.
func (n *Node) End() int64 { return n.Offset + n.Size }

// Find returns the first direct child of the given type, or nil.
func (n *Node) Find(typ string) *Node {
	for _, c := range n.Children {
		if c.Type == typ {
			return c
		}
	}
	return nil
}

// FindAll returns every direct child of the given type.
func (n *Node) FindAll(typ string) []*Node {
	var out []*Node
	for _, c := range n.Children {
		if c.Type == typ {
			out = append(out, c)
		}
	}
	return out
}

// FindPath walks a chain of child types, e.g. FindPath("moov","trak","mdia").
// The first element may match the receiver itself.
func (n *Node) FindPath(types ...string) *Node {
	cur := n
	for i, t := range types {
		if i == 0 && cur.Type == t {
			continue
		}
		cur = cur.Find(t)
		if cur == nil {
			return nil
		}
	}
	return cur
}

// Count returns the total number of nodes in the subtree, receiver included.
func (n *Node) Count() int {
	c := 1
	for _, ch := range n.Children {
		c += ch.Count()
	}
	return c
}

// Forest is a parsed file: the ordered list of top-level boxes.
type Forest struct {
	Roots []*Node
	Size  int64 // file size in bytes
}

// Find returns the first top-level box of the given type, or nil.
func (f *Forest) Find(typ string) *Node {
	for _, r := range f.Roots {
		if r.Type == typ {
			return r
		}
	}
	return nil
}

// Count returns the total number of boxes in the file.
func (f *Forest) Count() int {
	c := 0
	for _, r := range f.Roots {
		c += r.Count()
	}
	return c
}

// ---------------------------------------------------------------------------
// Cross-module data model: tracks, sample tables, edit lists, sample records.
// ---------------------------------------------------------------------------

// EditSegment is one elst entry. MediaTime == -1 marks an empty (gap)
// segment: presentation time passes with no media playing.
type EditSegment struct {
	SegmentDuration int64   // in movie (mvhd) timescale units
	MediaTime       int64   // media timescale units; -1 = empty segment
	MediaRate       float64 // normally 1.0
}

// Empty reports whether the segment is a gap.
func (e EditSegment) Empty() bool { return e.MediaTime < 0 }

// SampleRecord describes one sample's decode/presentation time and its
// on-disk location. DTS/PTS are raw media-timescale values; timeline
// normalization (wrap, edits) happens in internal/time.
type SampleRecord struct {
	Index    int    // 0-based sample number in decode order
	Offset   int64  // absolute file offset of the compressed sample
	Size     uint32 // compressed size in bytes
	DTS      int64  // decode time, media timescale units
	PTS      int64  // presentation time, media timescale units (DTS + ctts)
	Keyframe bool   // from stss; false when the track has no stss
}

// SampleTable is the fully expanded per-sample index of one track.
type SampleTable struct {
	Samples   []SampleRecord // decode order
	Keyframes []int          // sorted 0-based keyframe sample indices
	HasSTSS   bool           // false = no explicit sync-sample table
}

// SampleCount returns the number of samples.
func (t *SampleTable) SampleCount() int { return len(t.Samples) }

// KeyframeAtOrBefore returns the largest keyframe index whose sample
// number is <= sample, or -1 if none exists.
func (t *SampleTable) KeyframeAtOrBefore(sample int) int {
	lo, hi := 0, len(t.Keyframes)-1
	ans := -1
	for lo <= hi {
		mid := (lo + hi) / 2
		if t.Keyframes[mid] <= sample {
			ans = t.Keyframes[mid]
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return ans
}

// Track is one indexed trak.
type Track struct {
	ID        uint32
	Handler   string // "vide", "soun", or another 4cc
	Timescale uint32 // mdhd media timescale
	Duration  int64  // mdhd duration in media timescale units
	Edits     []EditSegment
	Table     *SampleTable
}

// IsVideo / IsAudio classify the track by handler type.
func (t *Track) IsVideo() bool { return t.Handler == "vide" }
func (t *Track) IsAudio() bool { return t.Handler == "soun" }
