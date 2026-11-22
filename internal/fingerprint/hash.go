// Package fingerprint computes content hashes of segments and detects
// repeated segments across fragments.
package fingerprint

import (
	"crypto/sha256"
	"encoding/binary"

	"mediaprobe/internal/segment"
)

// Hash computes a content fingerprint over sample sizes and payloads.
// Timestamps are deliberately excluded: identical content from different
// fragments must collide, and boundary or timeline-uncertain samples are
// never judged by their timestamps alone.
func Hash(seg *segment.Segment) [32]byte {
	h := sha256.New()
	var sz [4]byte
	for _, s := range seg.Samples {
		binary.LittleEndian.PutUint32(sz[:], s.Size)
		h.Write(sz[:])
		h.Write(s.Data)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// Hex returns the lowercase hex encoding of a hash.
func Hex(h [32]byte) string {
	const digits = "0123456789abcdef"
	b := make([]byte, 64)
	for i, v := range h {
		b[2*i], b[2*i+1] = digits[v>>4], digits[v&0xf]
	}
	return string(b)
}

// Deduper detects repeated segments by content hash.
type Deduper struct {
	seen map[[32]byte]int
	dups []int
}

// NewDeduper returns an empty deduper.
func NewDeduper() *Deduper {
	return &Deduper{seen: map[[32]byte]int{}}
}

// Add registers seg and returns the canonical segment ID plus whether seg
// duplicates an earlier segment.
func (d *Deduper) Add(seg *segment.Segment) (canonical int, dup bool) {
	h := Hash(seg)
	if id, ok := d.seen[h]; ok {
		d.dups = append(d.dups, seg.ID)
		return id, true
	}
	d.seen[h] = seg.ID
	return seg.ID, false
}

// Duplicates returns the IDs of segments found to be duplicates.
func (d *Deduper) Duplicates() []int { return d.dups }
