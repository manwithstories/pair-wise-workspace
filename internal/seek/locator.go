// Package seek locates the nearest decodable keyframe at or before a
// target presentation time and derives the sample window around it.
package seek

import (
	"fmt"

	"mp4loc/internal/box"
	"mp4loc/internal/time"
)

// LocateError reports a target that cannot be served. Reason is one of
// "before_first_segment", "empty_edit", "beyond_duration",
// "no_keyframe_table", "no_keyframe_before_target".
type LocateError struct {
	Reason    string
	Boundary  float64 // nearest usable presentation time, seconds
	TrackID   uint32
	TargetSec float64
}

func (e *LocateError) Error() string {
	return fmt.Sprintf("cannot locate %.3fs on track %d: %s (nearest boundary %.3fs)",
		e.TargetSec, e.TrackID, e.Reason, e.Boundary)
}

// Landing is the resolved seek result.
type Landing struct {
	Sample      int     // keyframe sample index (decode order)
	PTS         int64   // normalized media-time PTS of the landing sample
	PTSSeconds  float64 // PTS in seconds
	Offset      int64   // absolute file offset of the sample
	Size        uint32  // compressed sample size
	Window      []int   // sample indices of the before/after window
	TargetMedia int64   // normalized media time the target mapped to
}

// Locate resolves targetSec on the track described by tl. before/after
// size the sample window around the landing keyframe.
func Locate(tl *timeline.Timeline, targetSec float64, before, after int) (*Landing, error) {
	tr := tl.Track
	fail := func(reason string, boundaryPres int64) *LocateError {
		return &LocateError{
			Reason:    reason,
			Boundary:  float64(boundaryPres) / float64(tl.MovieTimescale),
			TrackID:   tr.ID,
			TargetSec: targetSec,
		}
	}
	if tr.Table == nil || tr.Table.SampleCount() == 0 {
		return nil, fail("no_samples", 0)
	}
	if tr.IsVideo() && !tr.Table.HasSTSS {
		// Never silently assume every frame is randomly decodable.
		return nil, fail("no_keyframe_table", 0)
	}
	pres := tl.SecondsToPres(targetSec)
	res := tl.MapToMedia(pres)
	if !res.OK {
		return nil, fail(res.Reason, res.Boundary)
	}
	target := res.MediaTime

	// Candidate keyframes: stss entries for video; every sample for
	// audio (intra-coded by definition). Pick the last whose normalized
	// PTS does not exceed the target.
	candidates := tr.Table.Keyframes
	if !tr.IsVideo() {
		candidates = make([]int, tr.Table.SampleCount())
		for i := range candidates {
			candidates[i] = i
		}
	}
	landing := -1
	for _, k := range candidates {
		pts := tl.MediaOrigin + tr.Table.Samples[k].PTS
		if pts <= target {
			landing = k
		} else {
			break
		}
	}
	if landing < 0 {
		first := candidates[0]
		boundaryPres := tl.MediaOrigin + tr.Table.Samples[first].PTS
		return nil, &LocateError{
			Reason:    "no_keyframe_before_target",
			Boundary:  tl.MediaToSeconds(boundaryPres),
			TrackID:   tr.ID,
			TargetSec: targetSec,
		}
	}

	n := tr.Table.SampleCount()
	lo := landing - before
	if lo < 0 {
		lo = 0
	}
	hi := landing + after
	if hi > n-1 {
		hi = n - 1
	}
	window := make([]int, 0, hi-lo+1)
	for i := lo; i <= hi; i++ {
		window = append(window, i)
	}
	s := tr.Table.Samples[landing]
	pts := tl.MediaOrigin + s.PTS
	return &Landing{
		Sample:      landing,
		PTS:         pts,
		PTSSeconds:  tl.MediaToSeconds(pts),
		Offset:      s.Offset,
		Size:        s.Size,
		Window:      window,
		TargetMedia: target,
	}, nil
}

// FrameType labels a sample for output.
func FrameType(rec *box.SampleRecord, hasSTSS bool, isVideo bool) string {
	if !isVideo {
		return "audio"
	}
	if rec.Keyframe {
		return "keyframe"
	}
	if !hasSTSS {
		return "unknown"
	}
	return "delta"
}
