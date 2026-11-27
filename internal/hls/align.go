// Package hls aligns multi-bitrate representations onto shared segment
// boundaries and writes deterministic HLS playlists. All boundary math
// runs on the unified 90 kHz timeline, so no variant ever needs a
// different cut point by more than one frame.
package hls

import (
	"sort"

	"fastpost/internal/model"
	"fastpost/internal/timeline"
)

// FrameTicks is the one-frame alignment tolerance on the 90 kHz
// timeline, taken at a conservative 30 fps (3000 ticks). Two candidate
// boundaries within this distance are snapped to a single shared cut.
const FrameTicks int64 = 3000

// Variant is one bitrate representation. Boundaries are its native cut
// points expressed in common 90 kHz ticks. Segments reference source
// blobs by name; the remuxer only re-wraps, it never re-encodes.
type Variant struct {
	Name       string
	Bandwidth  uint32
	Resolution string
	Codecs     string
	Boundaries []int64
	// SegmentFile names the source segment covering [start,end).
	SegmentFile func(seq int) string
}

// Bound is one shared, aligned segment boundary. Start is inclusive, End
// exclusive; both are common-tick timestamps.
type Bound struct {
	Seq   int
	Start int64
	End   int64
}

// Align computes one shared set of segment bounds covering every variant.
// Inputs must already be on the common timeline. Candidate cuts that lie
// within FrameTicks of each other are merged to the earliest tick so all
// bitrates share identical boundaries.
func Align(variants []Variant) ([]Bound, error) {
	col := &model.Collector{}
	cuts := collectCuts(variants, col)
	if col.Has() {
		return nil, col.Err()
	}
	if len(cuts) == 0 {
		return nil, nil
	}
	sort.Slice(cuts, func(i, j int) bool { return cuts[i] < cuts[j] })

	// Merge cuts within one frame into the earlier tick.
	var merged []int64
	for _, c := range cuts {
		if len(merged) == 0 || c-merged[len(merged)-1] > FrameTicks {
			merged = append(merged, c)
		}
	}
	// Turn cut points into [start,end) bounds. The first cut is the
	// timeline origin; consecutive cuts delimit one segment each.
	bounds := make([]Bound, 0, len(merged)-1)
	for i := 0; i+1 < len(merged); i++ {
		if merged[i+1] <= merged[i] {
			col.Add(model.FaultNegativeDuration, "align", merged[i],
				"non-increasing segment boundary")
			return nil, col.Err()
		}
		bounds = append(bounds, Bound{Seq: i, Start: merged[i], End: merged[i+1]})
	}
	return bounds, nil
}

// collectCuts gathers every distinct boundary tick from every variant,
// rejecting negative or unsorted input per variant.
func collectCuts(variants []Variant, col *model.Collector) []int64 {
	seen := map[int64]bool{}
	var cuts []int64
	for _, v := range variants {
		var prev int64 = -1
		for _, b := range v.Boundaries {
			if b < 0 {
				col.Add(model.FaultNegativeDuration, v.Name, b, "negative boundary tick")
				continue
			}
			if b < prev {
				col.Add(model.FaultUnmappableEvent, v.Name, b, "boundaries not sorted")
				continue
			}
			prev = b
			if !seen[b] {
				seen[b] = true
				cuts = append(cuts, b)
			}
		}
	}
	return cuts
}

// AssignEvent returns the sequence number of the segment that owns an
// event at the given common-tick timestamp. An event landing exactly on a
// boundary belongs to the LATER segment (half-open [start,end) intervals).
func AssignEvent(bounds []Bound, atTick int64) (int, bool) {
	idx := sort.Search(len(bounds), func(i int) bool {
		return bounds[i].End > atTick
	})
	if idx >= len(bounds) {
		return 0, false
	}
	if atTick < bounds[idx].Start {
		return 0, false
	}
	return bounds[idx].Seq, true
}

// Skew reports the largest residual offset, in common ticks, between a
// variant's native boundary and the shared bound it was snapped onto. A
// clean alignment returns zero because every input already shares cuts.
func Skew(shared []Bound, native []int64) int64 {
	var worst int64
	for _, n := range native {
		best := nearestDistance(shared, n)
		if best > worst {
			worst = best
		}
	}
	return worst
}

func nearestDistance(bounds []Bound, tick int64) int64 {
	var best int64 = 1<<63 - 1
	consider := func(c int64) {
		d := c - tick
		if d < 0 {
			d = -d
		}
		if d < best {
			best = d
		}
	}
	for _, b := range bounds {
		consider(b.Start)
		consider(b.End)
	}
	return best
}

// TicksPerSecond re-exports the common rate for playlist duration math.
func TicksPerSecond() int64 { return timeline.CommonRate }
