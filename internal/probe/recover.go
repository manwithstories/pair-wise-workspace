package probe

import (
	"sort"

	"mediacheck/internal/model"
)

// CutPlan is one desired cut on the shared timeline, expressed in ticks.
type CutPlan struct {
	At int64
}

// Boundary is the decodable cut actually chosen for a track. When no
// keyframe exists inside the search window, Gap is set with the exact tick
// span that cannot be produced; callers must never fabricate a segment.
type Boundary struct {
	PlanAt       int64
	ChosenAt     int64
	SampleIndex  uint32
	Gap          *model.TimeRange
	SearchWindow model.TimeRange
}

// DecodableRange marks a verified contiguous run of samples [From, To)
// (0-based, half-open). It is the recoverable prefix concept at track level.
type DecodableRange struct {
	From    uint32
	To      uint32
	Start   int64
	End     int64
}

// keyframeList returns sorted 0-based keyframe sample indices for a track.
// Audio samples are all independently decodable, so every index qualifies.
func keyframeList(tr *Track) []uint32 {
	var out []uint32
	if !tr.IsVideo {
		for i := range tr.Samples {
			out = append(out, uint32(i))
		}
		return out
	}
	for _, s := range tr.Samples {
		if s.IsKeyframe {
			out = append(out, s.Index)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ScanBoundaries searches a decodable keyframe at or before each planned
// cut, bounded by windowTicks on both sides. The chosen boundary is the
// nearest keyframe at or before PlanAt inside the window; if none exists
// the boundary is recorded as a gap with explicit tick coordinates.
func ScanBoundaries(tr *Track, plans []CutPlan, windowTicks int64) []Boundary {
	keys := keyframeList(tr)
	out := make([]Boundary, 0, len(plans))
	for _, p := range plans {
		win := model.TimeRange{Start: p.At - windowTicks, End: p.At + windowTicks}
		b := Boundary{PlanAt: p.At, SearchWindow: win}
		idx := nearestKeyBefore(keys, tr, p.At)
		if idx < 0 {
			b.Gap = &model.TimeRange{Start: win.Start, End: win.End}
			out = append(out, b)
			continue
		}
		chosen := tr.Samples[idx].StartTicks
		if chosen < win.Start || chosen > win.End {
			b.Gap = &model.TimeRange{Start: win.Start, End: win.End}
			out = append(out, b)
			continue
		}
		b.ChosenAt = chosen
		b.SampleIndex = uint32(idx)
		out = append(out, b)
	}
	return out
}

// nearestKeyBefore returns the sample index of the greatest keyframe whose
// StartTicks <= at, or -1 when the track has no earlier keyframe.
func nearestKeyBefore(keys []uint32, tr *Track, at int64) int {
	lo, hi := 0, len(keys)
	for lo < hi {
		mid := (lo + hi) / 2
		if tr.Samples[keys[mid]].StartTicks <= at {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == 0 {
		return -1
	}
	return int(keys[lo-1])
}

// RecoverablePrefix returns the longest verified sample run starting at 0.
// A corrupt/out-of-bounds sample ends the prefix; everything after the
// first invalid sample belongs to a quarantined tail.
func RecoverablePrefix(tr *Track, invalid map[uint32]struct{}) DecodableRange {
	if len(tr.Samples) == 0 {
		return DecodableRange{}
	}
	to := uint32(len(tr.Samples))
	for i := range tr.Samples {
		if _, bad := invalid[uint32(i)]; bad {
			to = uint32(i)
			break
		}
	}
	r := DecodableRange{From: 0, To: to, Start: tr.Samples[0].StartTicks}
	if to > 0 {
		r.End = tr.Samples[to-1].EndTicks
	}
	return r
}

// QuarantineTail converts the suffix after a recoverable prefix into the
// shared gap vocabulary, with explicit start/end ticks.
func QuarantineTail(tr *Track, prefix DecodableRange) *model.TimeRange {
	if int(prefix.To) >= len(tr.Samples) {
		return nil
	}
	return &model.TimeRange{
		Start: tr.Samples[prefix.To].StartTicks,
		End:   tr.Samples[len(tr.Samples)-1].EndTicks,
	}
}
