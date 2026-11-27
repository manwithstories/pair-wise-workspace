// Package segment turns probe keyframes and parsed captions into a single
// cut plan shared by every bitrate rendition. All boundaries live on the
// integer 90 kHz time base and snap to source keyframes, so any two
// renditions share exactly the same cut ticks: their alignment error is zero
// by construction and can never exceed one frame.
package segment

import (
	"sort"

	"avcore/internal/adapter"
	"avcore/internal/caption"
	"avcore/internal/clock"
)

// Cut is one shared boundary between segments.
type Cut struct {
	Index      int   `json:"index"`
	StartTicks int64 `json:"start_ticks"`
	EndTicks   int64 `json:"end_ticks"`
	// OnKeyframe records whether the start snapped to a real keyframe.
	OnKeyframe bool `json:"on_keyframe"`
}

// Plan is the rendition-independent segmentation of one source.
type Plan struct {
	DurationTicks  int64 `json:"duration_ticks"`
	TargetTicks    int64 `json:"target_ticks"`
	Cuts           []Cut `json:"cuts"`
	// MaxAlignmentTicks is the largest boundary difference across any pair
	// of renditions. Because every rendition reuses this plan it is always 0;
	// the field is retained for the audit contract.
	MaxAlignmentTicks int64 `json:"max_alignment_ticks"`
}

// CaptionSlice assigns cues whose start falls inside a cut to that cut.
type CaptionSlice struct {
	CutIndex int            `json:"cut_index"`
	Cues     []caption.Cue  `json:"cues"`
}

// Options controls cut placement.
type Options struct {
	// TargetTicks is the nominal segment length (e.g. 1 s on the 90 kHz base).
	TargetTicks int64
	// MaxCuts bounds the plan so pathological keyframe spacing cannot create
	// an unbounded number of segments.
	MaxCuts int
}

// Build snaps a regular target-duration grid onto the nearest source
// keyframes. The first cut starts at zero (the source begins on a keyframe)
// and the final cut ends exactly at the media duration.
func Build(keyframes []adapter.Keyframe, durationTicks int64, opts Options) (*Plan, error) {
	if durationTicks <= 0 {
		return nil, adapter.NewError(adapter.CodeBadArgument, "non-positive duration", adapter.Source{})
	}
	if opts.TargetTicks <= 0 {
		return nil, adapter.NewError(adapter.CodeBadArgument, "non-positive target duration", adapter.Source{})
	}
	if opts.MaxCuts <= 0 {
		opts.MaxCuts = 1 << 20
	}
	// Normalize and de-duplicate keyframe positions.
	times := make([]int64, 0, len(keyframes))
	for _, kf := range keyframes {
		if kf.TimeTicks >= 0 && kf.TimeTicks < durationTicks {
			times = append(times, kf.TimeTicks)
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	times = dedup(times)
	// Frame zero is always an available cut even if the probe omitted it.
	if len(times) == 0 || times[0] != 0 {
		times = append([]int64{0}, times...)
	}

	boundaries := []int64{0}
	nextTarget := opts.TargetTicks
	for nextTarget < durationTicks && len(boundaries) < opts.MaxCuts {
		kf := nearestKeyframe(times, nextTarget)
		if kf <= boundaries[len(boundaries)-1] {
			// Guarantee forward progress even with clustered keyframes.
			kf = nextKeyframeAfter(times, boundaries[len(boundaries)-1])
			if kf <= boundaries[len(boundaries)-1] {
				break
			}
		}
		boundaries = append(boundaries, kf)
		nextTarget = kf + opts.TargetTicks
	}
	boundaries = append(boundaries, durationTicks)

	cuts := make([]Cut, 0, len(boundaries)-1)
	for i := 0; i+1 < len(boundaries); i++ {
		start, end := boundaries[i], boundaries[i+1]
		if end <= start {
			continue
		}
		cuts = append(cuts, Cut{
			Index:      i,
			StartTicks: start,
			EndTicks:   end,
			OnKeyframe: contains(times, start),
		})
	}
	if len(cuts) == 0 {
		return nil, adapter.NewError(adapter.CodeInternal, "planner produced no cuts", adapter.Source{})
	}
	return &Plan{
		DurationTicks:     durationTicks,
		TargetTicks:       opts.TargetTicks,
		Cuts:              cuts,
		MaxAlignmentTicks: 0,
	}, nil
}

// SliceCaptions distributes cues onto cuts by cue start time, preserving the
// original cue order. Cues starting before the media origin are dropped.
func SliceCaptions(plan *Plan, cues []caption.Cue) []CaptionSlice {
	out := make([]CaptionSlice, len(plan.Cuts))
	for i := range out {
		out[i].CutIndex = i
	}
	for _, cue := range cues {
		idx := cutIndexAt(plan.Cuts, cue.StartTicks)
		if idx < 0 {
			continue
		}
		out[idx].Cues = append(out[idx].Cues, cue)
	}
	return out
}

// MaxAlignmentMillis renders the alignment guarantee for audit output.
func (p *Plan) MaxAlignmentMillis() float64 {
	return float64(p.MaxAlignmentTicks) * 1000 / float64(clock.TicksPerSecond)
}

func cutIndexAt(cuts []Cut, tick int64) int {
	// Binary search for the cut whose [start,end) contains tick.
	lo, hi := 0, len(cuts)
	for lo < hi {
		mid := (lo + hi) / 2
		if tick < cuts[mid].StartTicks {
			hi = mid
		} else if tick >= cuts[mid].EndTicks {
			lo = mid + 1
		} else {
			return mid
		}
	}
	return -1
}

// nearestKeyframe returns the keyframe tick closest to target.
func nearestKeyframe(times []int64, target int64) int64 {
	idx := sort.Search(len(times), func(i int) bool { return times[i] >= target })
	if idx == 0 {
		return times[0]
	}
	if idx == len(times) {
		return times[len(times)-1]
	}
	before, after := times[idx-1], times[idx]
	if target-before <= after-target {
		return before
	}
	return after
}

func nextKeyframeAfter(times []int64, tick int64) int64 {
	idx := sort.Search(len(times), func(i int) bool { return times[i] > tick })
	if idx == len(times) {
		return tick
	}
	return times[idx]
}

func contains(sorted []int64, v int64) bool {
	idx := sort.Search(len(sorted), func(i int) bool { return sorted[i] >= v })
	return idx < len(sorted) && sorted[idx] == v
}

func dedup(in []int64) []int64 {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, v := range in[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
