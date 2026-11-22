// Package split cuts validated sample tracks into delivery segments. Cut
// points snap to the nearest preceding random-access frame, VFR timing is
// accumulated from real sample durations, and events landing exactly on a
// boundary belong to exactly one segment via half-open interval ownership.
package split

import (
	"errors"
	"fmt"
	"time"

	"mediacq/internal/clock"
	"mediacq/internal/ingest"
)

// ErrNoKeyframe means no random-access point can anchor a segment.
var ErrNoKeyframe = errors.New("split: no keyframe available for cut alignment")

// Config controls segmentation.
type Config struct {
	TargetDurationNS int64
	// MaxDriftWindows flag segments intersecting these ranges.
	DriftWindows []clock.DriftWindow
}

// Segment is one cut, expressed on the unified timeline.
type Segment struct {
	Seq            int
	Range          clock.Range
	SampleStart    int // first video sample index (inclusive)
	SampleEnd      int // one past last video sample index
	KeyframeIndex  int
	EventIDs       []uint32
	DriftAffected  bool
}

// Plan holds the deterministic cut result.
type Plan struct {
	Segments []Segment
}

// Cut partitions a video track into segments of approximately the target
// duration, snapping every boundary to a keyframe.
func Cut(video *ingest.Track, events []ingest.Event, cfg Config) (*Plan, error) {
	if len(video.Samples) == 0 {
		return nil, errors.New("split: empty video track")
	}
	if len(video.KeyframeIndices) == 0 {
		return nil, fmt.Errorf("%w: track=%d", ErrNoKeyframe, video.ID)
	}
	samples := video.Samples
	keys := video.KeyframeIndices
	plan := &Plan{}

	startSample := 0
	seq := 0
	for startSample < len(samples) {
		startTime := samples[startSample].Start
		idealEnd := clock.Time(startTime.Nanoseconds() + cfg.TargetDurationNS)

		endSample, isTail := pickCutSample(samples, keys, startSample, idealEnd)

		var endTime clock.Time
		if isTail {
			// The tail consumes every remaining sample; its end is the final
			// sample's start plus its own duration.
			last := samples[len(samples)-1]
			t, err := last.Start.Add(time.Duration(last.Duration))
			if err != nil {
				return nil, err
			}
			endTime = t
		} else {
			endTime = samples[endSample].Start
		}
		seg := Segment{
			Seq:           seq,
			SampleStart:   startSample,
			SampleEnd:     endSample,
			KeyframeIndex: startSample,
			Range: clock.Range{
				Start: samples[startSample].Start,
				End:   endTime,
			},
		}
		seg.EventIDs = assignEvents(events, seg.Range)
		seg.DriftAffected = intersectsAny(seg.Range, cfg.DriftWindows)
		plan.Segments = append(plan.Segments, seg)

		seq++
		startSample = endSample
	}
	return plan, nil
}

// pickCutSample returns the exclusive end sample index (always a keyframe for
// a non-tail cut) and reports whether this is the final tail segment.
func pickCutSample(samples []ingest.Sample, keys []int, start int, idealEnd clock.Time) (int, bool) {
	last := len(samples)
	// Greatest keyframe strictly after start whose start time is <= ideal end.
	best := -1
	for _, k := range keys {
		if k <= start {
			continue
		}
		if samples[k].Start.Nanoseconds() <= idealEnd.Nanoseconds() {
			best = k
		} else {
			break
		}
	}
	if best < 0 {
		// No keyframe inside the target window: advance to the next keyframe,
		// guaranteeing every segment begins on a random-access point.
		for _, k := range keys {
			if k > start {
				return k, k == last
			}
		}
		return last, true
	}
	// If the chosen keyframe is the last one, everything remaining is the tail.
	if best == keys[len(keys)-1] {
		return last, true
	}
	return best, false
}

// assignEvents returns events strictly inside the half-open segment range.
// An event exactly on Range.End is excluded here and owned by the next segment.
func assignEvents(events []ingest.Event, r clock.Range) []uint32 {
	out := make([]uint32, 0)
	for _, e := range events {
		if r.Contains(e.Time) {
			out = append(out, e.ID)
		}
	}
	return out
}

func intersectsAny(r clock.Range, windows []clock.DriftWindow) bool {
	for _, w := range windows {
		if r.Overlaps(w.Range) {
			return true
		}
	}
	return false
}
