// Package segments aligns segment boundaries across bitrate representations
// of the same program.  The primary representation decides the boundaries;
// every other representation must have a keyframe within one frame of each
// boundary or the alignment fails with the segment ordinal and the tracks
// involved.  Boundaries are rational ticks, so the result is reproducible.
package segments

import (
	"fmt"

	"mediapack/internal/clock"
	"mediapack/internal/fault"
	"mediapack/internal/index"
)

// Limits bounds the work.
type Limits struct{ MaxSegments int }

// DefaultLimits permits an hour of one-second segments.
func DefaultLimits() Limits { return Limits{MaxSegments: 36000} }

// Representation is one bitrate layer of the program.
type Representation struct {
	Name string
	Rate int
	// FrameDuration is the nominal frame duration of this layer, used as the
	// one-frame tolerance.
	FrameDuration clock.Ticks
	Keyframes     *index.Table
}

// Segment is one aligned output segment.
type Segment struct {
	Ordinal int
	Start   clock.Ticks
	End     clock.Ticks
	// Keyframes maps representation name to the keyframe that starts the
	// segment in that layer.
	Keyframes map[string]index.Keyframe
	// FrameCount is the number of frames in the primary layer.
	FrameCount int
}

// Plan is the aligned segmentation.
type Plan struct {
	Segments []Segment
	Primary  string
	// MaxDeviation is the largest boundary offset seen, in ticks.
	MaxDeviation clock.Ticks
}

// Align builds the segment plan.  reps[0] is the primary representation.
func Align(reps []Representation, segmentTicks clock.Ticks, limits Limits) (*Plan, error) {
	if len(reps) == 0 {
		return nil, fault.New(fault.ClassInput, "segments", "*", "no-representations",
			"at least one representation is required")
	}
	if segmentTicks.Sign() <= 0 {
		return nil, fault.New(fault.ClassUsage, "segments", "*", "invalid-segment-size",
			"segment size must be positive")
	}
	for _, r := range reps {
		if r.Keyframes == nil || len(r.Keyframes.Keyframes) == 0 {
			return nil, fault.New(fault.ClassDependency, "segments", r.Name, "missing-index",
				fmt.Sprintf("representation %s has no keyframe index", r.Name)).
				WithBlockedBy("index:" + r.Name)
		}
	}

	primary := reps[0]
	duration := primary.Keyframes.Duration()
	if duration.Sign() <= 0 {
		return nil, fault.New(fault.ClassInput, "segments", primary.Name, "empty-track",
			"primary representation has zero duration")
	}

	// Boundaries are at 0, segmentTicks, 2*segmentTicks, ... up to duration.
	// A trailing segment is emitted only when the tail holds at least one
	// frame; otherwise the plan would claim a zero-length segment.
	var bounds []clock.Ticks
	for t := clock.MustNew(0, 1); t.Cmp(duration) < 0; {
		bounds = append(bounds, t)
		next, err := t.Add(segmentTicks)
		if err != nil {
			return nil, fault.Wrap(fault.ClassInput, "segments", primary.Name,
				"time-overflow", "segment boundary overflows the tick range", err)
		}
		t = next
	}
	if len(bounds) == 0 {
		// The program is shorter than one segment: one segment covers it all.
		bounds = []clock.Ticks{clock.MustNew(0, 1)}
	}
	if len(bounds) > limits.MaxSegments {
		return nil, fault.New(fault.ClassLimit, "segments", primary.Name, "too-many-segments",
			fmt.Sprintf("%d segments exceeds limit %d", len(bounds), limits.MaxSegments))
	}

	plan := &Plan{Primary: primary.Name}
	for i, start := range bounds {
		end := duration
		if i+1 < len(bounds) {
			end = bounds[i+1]
		} else if tail, err := start.Add(segmentTicks); err == nil && tail.Cmp(duration) < 0 {
			end = tail
		}
		seg := Segment{Ordinal: i + 1, Start: start, End: end,
			Keyframes: make(map[string]index.Keyframe, len(reps))}

		for _, r := range reps {
			kf, deviation, err := keyframeAt(r, start, primary.FrameDuration)
			if err != nil {
				if fe, ok := fault.As(err); ok {
					fe.WithSegment(i + 1)
				}
				return nil, err
			}
			if i > 0 && deviation.Cmp(primary.FrameDuration) > 0 {
				return nil, fault.New(fault.ClassDependency, "segments", r.Name, "boundary-misaligned",
					fmt.Sprintf("segment %d: %s keyframe %s deviates %s from boundary %s (tolerance %s)",
						i+1, r.Name, kf.Ticks, deviation, start, primary.FrameDuration)).
					WithSegment(i + 1).WithBlockedBy("index:" + r.Name)
			}
			seg.Keyframes[r.Name] = kf
			if deviation.Cmp(plan.MaxDeviation) > 0 {
				plan.MaxDeviation = deviation
			}
		}
		if kf := primary.Keyframes.Keyframes; len(kf) > 0 {
			seg.FrameCount = framesBetween(primary.Keyframes, start, end)
		}
		plan.Segments = append(plan.Segments, seg)
	}
	return plan, nil
}

// keyframeAt returns the latest keyframe at or before t and its distance to t.
func keyframeAt(r Representation, t clock.Ticks, tolerance clock.Ticks) (index.Keyframe, clock.Ticks, error) {
	kf, ok := r.Keyframes.At(t)
	if ok {
		dev, err := t.Sub(kf.Ticks)
		if err != nil {
			return kf, clock.Ticks{}, fault.Wrap(fault.ClassInput, "segments", r.Name,
				"time-overflow", "boundary deviation overflows", err)
		}
		return kf, dev, nil
	}
	// No keyframe at or before t.  That is tolerable only at the very start,
	// where the first keyframe may sit within one frame after zero because
	// the representations disagree about the first sample's start; at any
	// later boundary it is a genuine misalignment.
	first := r.Keyframes.Keyframes[0]
	if t.IsZero() {
		if dev, err := first.Ticks.Sub(t); err == nil && dev.Cmp(tolerance) <= 0 {
			return first, dev, nil
		}
	}
	return index.Keyframe{}, clock.Ticks{}, fault.New(fault.ClassDependency, "segments", r.Name,
		"boundary-misaligned", fmt.Sprintf("no keyframe at or before boundary %s", t))
}

// framesBetween counts primary frames whose start lies in [start, end).
func framesBetween(table *index.Table, start, end clock.Ticks) int {
	n := 0
	for _, s := range table.Starts {
		if s.Cmp(start) >= 0 && s.Cmp(end) < 0 {
			n++
		}
	}
	return n
}

// Summary renders a compact description of the plan.
func (p *Plan) Summary() string {
	return fmt.Sprintf("%d segments, primary=%s, max_deviation=%s", len(p.Segments), p.Primary, p.MaxDeviation)
}
