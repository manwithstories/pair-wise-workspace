// Package segment aligns multiple bitrate representations onto shared cut
// points and emits the segment manifest.
//
// All representations must cut at the same logical boundary: a cut is only
// valid where every representation has a keyframe. When a representation has
// no keyframe at the ideal time, the boundary is extended to that
// representation's nearest keyframe and the resulting deviation is recorded.
// A deviation beyond the configured tolerance fails loudly with E_NO_KEYFRAME
// instead of silently producing misaligned segments.
package segment

import (
	"sort"

	"mediad/diag"
	"mediad/timeline"
)

// Frame is one alignable media frame on the unified time base.
type Frame struct {
	Time     timeline.Rat
	Keyframe bool
}

// Representation is one bitrate layer's ordered frame list.
type Representation struct {
	ID      string
	Bitrate int
	Frames  []Frame
}

// Cut is one representation's actual boundary for a segment.
type Cut struct {
	FrameIndex  int
	ActualStart timeline.Rat
	Keyframe    bool
	// DeviationMS is the signed, rounded difference from the common boundary.
	DeviationMS int64
}

// Segment is one manifest entry shared by every representation.
type Segment struct {
	Index int
	Start timeline.Rat
	End   timeline.Rat
	Cuts  map[string]Cut
}

// Policy controls boundary extension behavior.
type Policy struct {
	// TargetMS is the ideal segment duration.
	TargetMS int64
	// MaxDeviationMS is the largest tolerated keyframe distance from the
	// common boundary; larger gaps fail with E_NO_KEYFRAME.
	MaxDeviationMS int64
}

// Manifest is the deterministic, hashable segment list.
type Manifest struct {
	Representations    []string
	TargetMS           int64
	MaxAlignmentErrMS  int64
	Segments           []Segment
}

// Align builds the shared manifest. Representations must be non-empty and
// share a common time base; their frame lists must be time ordered.
func Align(reps []Representation, pol Policy) (*Manifest, error) {
	if len(reps) == 0 {
		return nil, diag.New(diag.EParam, "", "no representations to align")
	}
	if pol.TargetMS <= 0 {
		return nil, diag.New(diag.EParam, "", "segment target must be positive, got %dms", pol.TargetMS)
	}
	ids := make([]string, 0, len(reps))
	for _, r := range reps {
		if len(r.Frames) == 0 {
			return nil, diag.New(diag.ENoKeyframe, r.ID, "representation has no frames")
		}
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)

	anchor := reps[0]
	endTime := lastFrameTime(reps)
	var segs []Segment
	maxErr := int64(0)
	ideal := int64(0)
	idx := 0
	for {
		idealTime := timeline.FromMillis(ideal)
		if idealTime.Cmp(endTime) >= 0 {
			break
		}
		anchorCut, err := firstKeyframeAtOrAfter(anchor, idealTime)
		if err != nil {
			return nil, err
		}
		common := anchor.Frames[anchorCut].Time
		seg := Segment{Index: idx, Start: common, Cuts: map[string]Cut{}}
		worst := int64(0)
		for _, r := range reps {
			cutIdx, cut, err := nearestKeyframe(r, common)
			if err != nil {
				return nil, err
			}
			dev := cut.Time.Sub(common).Millis()
			if dev < 0 {
				dev = -dev
			}
			if dev > pol.MaxDeviationMS {
				return nil, diag.New(diag.ENoKeyframe, r.ID,
					"keyframe deviation %dms from boundary %s exceeds tolerance %dms",
					dev, common.String(), pol.MaxDeviationMS)
			}
			seg.Cuts[r.ID] = Cut{FrameIndex: cutIdx, ActualStart: cut.Time, Keyframe: true, DeviationMS: dev}
			if dev > worst {
				worst = dev
			}
		}
		if worst > maxErr {
			maxErr = worst
		}
		segs = append(segs, seg)
		idx++
		ideal += pol.TargetMS
	}
	// Close every segment at the next segment's start; the final one ends at
	// the last frame seen across representations.
	for i := range segs {
		if i+1 < len(segs) {
			segs[i].End = segs[i+1].Start
		} else {
			segs[i].End = endTime
		}
	}
	return &Manifest{
		Representations:   ids,
		TargetMS:          pol.TargetMS,
		MaxAlignmentErrMS: maxErr,
		Segments:          segs,
	}, nil
}

// firstKeyframeAtOrAfter returns the index of the anchor's first keyframe at
// or after t; failing that, the last frame is used to extend the boundary.
func firstKeyframeAtOrAfter(r Representation, t timeline.Rat) (int, error) {
	for i, f := range r.Frames {
		if f.Keyframe && f.Time.Cmp(t) >= 0 {
			return i, nil
		}
	}
	return 0, diag.New(diag.ENoKeyframe, r.ID,
		"no keyframe at or after %s", t.String())
}

// nearestKeyframe returns the keyframe closest to the common boundary time.
func nearestKeyframe(r Representation, common timeline.Rat) (int, Frame, error) {
	bestIdx, best := -1, Frame{}
	bestDiff := int64(1<<62)
	for i, f := range r.Frames {
		if !f.Keyframe {
			continue
		}
		diff := f.Time.Sub(common).Millis()
		if diff < 0 {
			diff = -diff
		}
		if diff < bestDiff {
			bestDiff, bestIdx, best = diff, i, f
		}
	}
	if bestIdx < 0 {
		return 0, Frame{}, diag.New(diag.ENoKeyframe, r.ID, "representation contains no keyframes")
	}
	return bestIdx, best, nil
}

func lastFrameTime(reps []Representation) timeline.Rat {
	var last timeline.Rat
	for _, r := range reps {
		t := r.Frames[len(r.Frames)-1].Time
		if last.IsZero() || t.Cmp(last) > 0 {
			last = t
		}
	}
	return last
}
