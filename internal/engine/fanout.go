package engine

import (
	"sort"

	"github.com/fieldcut/fieldcut/internal/base"
	"github.com/fieldcut/fieldcut/internal/ingest"
)

// TargetSegmentsPerRendition is the fragment count each rendition produces
// for the acceptance program; six rendition streams therefore emit 144.
const TargetSegmentsPerRendition = 24

// SegmentDuration90k is the default target fragment duration (0.8 s); the
// builder overrides it from the program length when an exact count matters.
const SegmentDuration90k = base.ClockRate * 8 / 10

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
// rendition cuts at the identical set of boundaries, and each position is a
// keyframe for every rendition (except gap markers, which forbid splicing).
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

// FanOut computes the common time boundaries using the default segment
// duration.
func FanOut(streams []*Stream) *Plan {
	return FanOutWithDuration(streams, SegmentDuration90k)
}

// FanOutWithDuration computes the common time boundaries with an explicit
// target segment duration. A boundary opens only at a position that is a
// structural keyframe in EVERY rendition, so every produced segment starts
// independently decodable. Clock-gap units force a boundary that no
// rendition may splice across.
func FanOutWithDuration(streams []*Stream, segmentDuration uint64) *Plan {
	// Positions every rendition must cut at: keyframe union points and gap
	// points, intersected by requiring each rendition's next keyframe.
	gapPTS := map[uint64]bool{}
	var maxPTS uint64
	for _, s := range streams {
		for _, u := range s.Units {
			if u.ClockGap {
				gapPTS[u.PTS90k] = true
			}
			if u.PTS90k > maxPTS {
				maxPTS = u.PTS90k
			}
		}
	}

	// Walk a fixed target grid. At each grid point find each rendition's
	// first structural keyframe at or after it; the slowest rendition's
	// keyframe PTS becomes the shared boundary. A fixed iteration count
	// guarantees the tail segment is never dropped.
	var bounds []Boundary
	bounds = append(bounds, Boundary{Index: 0, PTS90k: 0, Keyframe: true})
	target := segmentDuration
	for target <= maxPTS {
		var boundaryPTS uint64
		allKey := true
		for _, s := range streams {
			kf := firstKeyframeAtOrAfter(s.Units, target)
			if kf == nil {
				allKey = false
				break
			}
			if kf.PTS90k > boundaryPTS {
				boundaryPTS = kf.PTS90k
			}
		}
		if !allKey {
			break
		}
		if boundaryPTS <= bounds[len(bounds)-1].PTS90k {
			// No forward progress: stop rather than loop forever.
			break
		}
		bounds = append(bounds, Boundary{
			Index: len(bounds), PTS90k: boundaryPTS, Keyframe: true,
			Gap: gapPTS[boundaryPTS],
		})
		target += segmentDuration
	}
	for i := range bounds {
		bounds[i].Index = i
	}

	// Assign unit spans per rendition by linear scan over sorted PTS.
	plan := &Plan{Boundaries: bounds, Ranges: make(map[string][][2]int, len(streams))}
	for _, s := range streams {
		ranges := make([][2]int, len(bounds))
		lo := 0
		for bi := range bounds {
			hi := lo
			if bi+1 < len(bounds) {
				next := bounds[bi+1].PTS90k
				for hi < len(s.Units) && s.Units[hi].PTS90k < next {
					hi++
				}
			} else {
				hi = len(s.Units)
			}
			ranges[bi] = [2]int{lo, hi}
			lo = hi
		}
		plan.Ranges[s.Rendition] = ranges
	}
	return plan
}

// firstKeyframeAtOrAfter returns the earliest keyframe unit with PTS >= pts.
func firstKeyframeAtOrAfter(units []*ingest.AccessUnit, pts uint64) *ingest.AccessUnit {
	idx := sort.Search(len(units), func(i int) bool { return units[i].PTS90k >= pts })
	for i := idx; i < len(units); i++ {
		if units[i].Keyframe {
			return units[i]
		}
	}
	return nil
}
