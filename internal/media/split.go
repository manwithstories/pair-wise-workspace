// Package media cuts frames on the unified timebase, detects multi-channel
// silence and fingerprints windows for cross-node deduplication.
package media

import (
	"fmt"

	"medrelay/internal/timeline"
)

// Frame is one audio or video access unit on its own timescale.
type Frame struct {
	Kind byte // 'a' or 'v'
	PTS  int64
	Dur  int64 // ticks
	TS   timeline.Timescale
	Data []byte
}

// Segment is a forwardable run of frames cut on representable boundaries.
type Segment struct {
	StartMs int64
	EndMs   int64
	Frames  []Frame
}

// Split cuts frames at the given cut points (milliseconds, unified clock).
// Every cut must land exactly on a frame start of every kind present;
// otherwise the cut is unrepresentable and an error locates the interval.
func Split(frames []Frame, cutsMs []int64) ([]Segment, error) {
	var segs []Segment
	prevMs := int64(0)
	idx := 0
	cuts := append(append([]int64{}, cutsMs...), -1) // -1 = end sentinel
	for _, cut := range cuts {
		endMs := cut
		if endMs < 0 {
			endMs = 1 << 62
		}
		var seg Segment
		seg.StartMs = prevMs
		for idx < len(frames) {
			f := frames[idx]
			fStart := timeline.ToMillis(f.PTS, f.TS)
			if fStart >= endMs {
				break
			}
			seg.Frames = append(seg.Frames, f)
			idx++
		}
		if len(seg.Frames) > 0 {
			last := seg.Frames[len(seg.Frames)-1]
			seg.EndMs = timeline.ToMillis(last.PTS+last.Dur, last.TS)
		} else {
			seg.EndMs = prevMs
		}
		segs = append(segs, seg)
		if cut < 0 {
			break
		}
		// The cut must be a frame start for every kind present.
		if err := checkBoundary(frames, cut); err != nil {
			return nil, err
		}
		prevMs = cut
	}
	return segs, nil
}

// checkBoundary verifies that cutMs coincides with a frame boundary of
// every frame kind, so no frame straddles the cut.
func checkBoundary(frames []Frame, cutMs int64) error {
	kinds := map[byte]bool{}
	for _, f := range frames {
		kinds[f.Kind] = true
	}
	for k := range kinds {
		ok := false
		var lo, hi int64 = -(1 << 62), 1 << 62
		for _, f := range frames {
			if f.Kind != k {
				continue
			}
			s := timeline.ToMillis(f.PTS, f.TS)
			e := timeline.ToMillis(f.PTS+f.Dur, f.TS)
			if s == cutMs {
				ok = true
				break
			}
			if s < cutMs && s > lo {
				lo = s
			}
			if e > cutMs && hi > e {
				hi = e
			}
		}
		if !ok {
			return timeline.New(timeline.ErrBoundary, fmt.Sprintf("kind=%c", k), lo, hi,
				fmt.Sprintf("cut at %dms is not a representable frame boundary", cutMs))
		}
	}
	return nil
}
