// Package quality runs cut detection, silence detection and content
// fingerprinting on the normalized timeline. All three analyses share the
// clock package's microsecond frame model and the timeline error model.
package quality

import (
	"sort"

	"mediaflow/internal/clock"
	"mediaflow/internal/timeline"
)

// CutPoint marks a timeline position where a segment boundary must be placed.
type CutPoint struct {
	AtUS       int64
	Track      uint32
	Reason     string
	FrameIndex int
}

// Segment is a half-open normalized range built from cut points. VFR frame
// durations are preserved because boundaries snap to actual frame starts.
type Segment struct {
	Index      int
	StartUS    int64
	EndUS      int64
	StartFrame int
	EndFrame   int // exclusive
}

// CutConfig controls cut placement.
type CutConfig struct {
	// MinSegmentUS is the shortest legal segment; closer cuts are merged.
	MinSegmentUS int64
	// HintUS are externally requested boundaries (chapter/event aligned).
	HintUS []int64
}

// DefaultCutConfig returns conservative demo bounds.
func DefaultCutConfig() CutConfig {
	return CutConfig{MinSegmentUS: 250_000}
}

// DetectCuts builds cut points from explicit hints and frame discontinuities,
// then splits the normalized frames of one track into VFR-aware segments.
func DetectCuts(frames []clock.Normalized, cfg CutConfig) ([]CutPoint, []Segment, error) {
	if len(frames) == 0 {
		return nil, nil, nil
	}
	track := frames[0].Track
	cuts := make([]CutPoint, 0)

	// A discontinuity (gap larger than the frame's own duration) is a natural
	// cut. Comparison uses normalized microseconds, never native ticks.
	for i := 1; i < len(frames); i++ {
		prev, cur := frames[i-1], frames[i]
		gap := cur.StartUS - (prev.StartUS + prev.Duration)
		if gap > prev.Duration {
			cuts = append(cuts, CutPoint{AtUS: cur.StartUS, Track: track,
				Reason: "discontinuity", FrameIndex: i})
		}
	}
	// Snap external hints to the nearest frame start without duplicating.
	for _, hint := range cfg.HintUS {
		idx := nearestFrame(frames, hint)
		if idx < 0 {
			continue
		}
		at := frames[idx].StartUS
		if !hasCut(cuts, at) {
			cuts = append(cuts, CutPoint{AtUS: at, Track: track, Reason: "hint", FrameIndex: idx})
		}
	}
	sort.Slice(cuts, func(i, j int) bool { return cuts[i].AtUS < cuts[j].AtUS })

	segments := splitAtCuts(frames, cuts, cfg.MinSegmentUS)
	if err := validateSegments(segments); err != nil {
		return nil, nil, err
	}
	return cuts, segments, nil
}

// nearestFrame returns the index of the frame whose start is closest to target.
func nearestFrame(frames []clock.Normalized, target int64) int {
	best, bestDist := -1, int64(1<<62)
	for i, f := range frames {
		d := f.StartUS - target
		if d < 0 {
			d = -d
		}
		if d < bestDist {
			best, bestDist = i, d
		}
	}
	return best
}

func hasCut(cuts []CutPoint, at int64) bool {
	for _, c := range cuts {
		if c.AtUS == at {
			return true
		}
	}
	return false
}

// splitAtCuts walks frames and opens a new segment at every cut. A segment
// shorter than the minimum is only merged into its predecessor when it was
// created by an explicit hint; a real discontinuity boundary is always kept.
// VFR durations are carried verbatim.
func splitAtCuts(frames []clock.Normalized, cuts []CutPoint, minSegUS int64) []Segment {
	cutAt := map[int64]CutPoint{}
	for _, c := range cuts {
		cutAt[c.AtUS] = c
	}
	var segments []Segment
	startFrame := 0
	closeAt := func(endFrame int) {
		startUS := frames[startFrame].StartUS
		endUS := frames[endFrame-1].StartUS + frames[endFrame-1].Duration
		seg := Segment{
			Index: len(segments), StartUS: startUS, EndUS: endUS,
			StartFrame: startFrame, EndFrame: endFrame,
		}
		if len(segments) > 0 && seg.SegmentDurationUS() < minSegUS {
			// Only hint-driven undersized segments merge; discontinuities stay.
			if cp, ok := cutAt[startUS]; ok && cp.Reason == "hint" {
				last := &segments[len(segments)-1]
				last.EndUS = endUS
				last.EndFrame = endFrame
				startFrame = endFrame
				return
			}
		}
		segments = append(segments, seg)
		startFrame = endFrame
	}
	for i := 1; i < len(frames); i++ {
		if hasCutAt(cutAt, frames[i].StartUS) {
			closeAt(i)
		}
	}
	closeAt(len(frames))
	return segments
}

func hasCutAt(cutAt map[int64]CutPoint, at int64) bool {
	_, ok := cutAt[at]
	return ok
}

func validateSegments(segments []Segment) error {
	for _, s := range segments {
		if _, err := timeline.NewInterval(s.StartUS, s.EndUS); err != nil {
			return timeline.GlobalError("quality.cut", true, err)
		}
	}
	return nil
}

// SegmentDurationUS exposes a segment length for reporting.
func (s Segment) SegmentDurationUS() int64 { return s.EndUS - s.StartUS }
