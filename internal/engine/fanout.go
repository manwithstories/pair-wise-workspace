// Package engine fans aligned access units out into per-rendition
// representations, fragments them into keyframe-aligned MP4 segments and
// supports atomic checkpointed recovery.
package engine

import (
	"sort"

	"github.com/fieldcut/fieldcut/internal/base"
	"github.com/fieldcut/fieldcut/internal/ingest"
)

// SegmentDuration90k is the target fragment duration on the shared clock
// (0.5 s). Actual boundaries snap to the next keyframe.
const SegmentDuration90k = base.ClockRate / 2

// Stream is one rendition's scanned access units plus its manifest identity.
type Stream struct {
	Camera     string
	Rendition  string
	Codec      ingest.Codec
	Color      ColorTag
	Units      []*ingest.AccessUnit
	ClockStats base.ClockSnapshot
}

// ColorTag is the color labeling carried by a rendition.
type ColorTag struct {
	Primaries uint16
	Transfer  uint16
	Matrix    uint16
	Source    string // manifest / bitstream / policy
}

// Boundary is one shared cut position expressed on the 90 kHz clock. Every
// rendition must cut at the identical set of boundaries.
type Boundary struct {
	Index    int
	PTS90k   uint64
	Keyframe bool
	Gap      bool
}

// Plan is the fan-out result: shared boundaries and per-rendition unit
// ranges referencing them.
type Plan struct {
	Boundaries []Boundary
	// Ranges[rendition][boundaryIndex] gives the unit index span [lo, hi).
	Ranges map[string][][2]int
}

// FanOut computes the common time boundaries from the union of rendition
// keyframes and assigns every access unit to a segment. Units marked with a
// clock gap force a boundary that no rendition may splice across.
func FanOut(streams []*Stream) *Plan {
	// 1. Collect candidate cut points: target grid plus every keyframe and
	// every gap marker from every rendition.
	cuts := map[uint64]struct{}{0: {}}
	var maxPTS uint64
	for _, s := range streams {
		for _, u := range s.Units {
			if u.Keyframe || u.ClockGap {
				cuts[u.PTS90k] = struct{}{}
			}
			if u.PTS90k > maxPTS {
				maxPTS = u.PTS90k
			}
		}
	}
	for g := SegmentDuration90k; g < maxPTS; g += SegmentDuration90k {
		cuts[g] = struct{}{}
	}
	sorted := make([]uint64, 0, len(cuts))
	for p := range cuts {
		sorted = append(sorted, p)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	// 2. Snap grid-only cuts to the next keyframe shared by all renditions;
	// keyframe and gap cuts are already real positions.
	bounds := make([]Boundary, 0, len(sorted)+1)
	for i, p := range sorted {
		gap := false
		key := true
		for _, s := range streams {
			u := unitAtOrBefore(s.Units, p)
			if u == nil {
				key = false
				break
			}
			if u.ClockGap {
				gap = true
			}
			if !u.Keyframe && !gap {
				// Snap forward to this rendition's next keyframe.
				if npts := nextKeyframePTS(s.Units, p); npts >= 0 && uint64(npts) <= maxPTS {
					p = uint64(npts)
				}
			}
		}
		bounds = append(bounds, Boundary{Index: i, PTS90k: p, Keyframe: key, Gap: gap})
	}
	// Deduplicate positions produced by snapping.
	bounds = dedupe(bounds)
	for i := range bounds {
		bounds[i].Index = i
	}

	// 3. Assign unit spans per rendition by binary search on PTS.
	plan := &Plan{Boundaries: bounds, Ranges: make(map[string][][2]int, len(streams))}
	for _, s := range streams {
		ranges := make([][2]int, len(bounds))
		lo := 0
		for bi, b := range bounds {
			hi := lo
			for hi < len(s.Units) && s.Units[hi].PTS90k < b.PTS90k {
				hi++
			}
			if bi == len(bounds)-1 {
				hi = len(s.Units)
			}
			ranges[bi] = [2]int{lo, hi}
			lo = hi
		}
		plan.Ranges[s.Rendition] = ranges
	}
	return plan
}

func unitAtOrBefore(units []*ingest.AccessUnit, pts uint64) *ingest.AccessUnit {
	idx := sort.Search(len(units), func(i int) bool { return units[i].PTS90k > pts }) - 1
	if idx < 0 {
		return nil
	}
	return units[idx]
}

func nextKeyframePTS(units []*ingest.AccessUnit, after uint64) int64 {
	idx := sort.Search(len(units), func(i int) bool { return units[i].PTS90k >= after })
	for i := idx; i < len(units); i++ {
		if units[i].Keyframe {
			return int64(units[i].PTS90k)
		}
	}
	return -1
}

func dedupe(b []Boundary) []Boundary {
	out := b[:0]
	for _, x := range b {
		if len(out) > 0 && out[len(out)-1].PTS90k == x.PTS90k {
			if x.Gap {
				out[len(out)-1].Gap = true
			}
			continue
		}
		out = append(out, x)
	}
	return out
}
