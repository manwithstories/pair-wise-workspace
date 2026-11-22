// Package segment computes common cut points across representations
// and emits the segment manifest. Cut points are exact: a grid point
// that does not land on a keyframe and an audio sample boundary in
// every representation is dropped, never rounded.
package segment

import (
	"fmt"

	"mediac/internal/clock"
	"mediac/internal/model"
	"mediac/internal/probe"
)

// CommonCuts returns the shared cut points (including 0 and the common
// duration) for 2–4 representations. A grid point is kept only when it
// is a video keyframe timestamp and an exact audio frame boundary in
// every representation — no silent rounding of the timeline.
func CommonCuts(reps []*probe.Result, segDur clock.Rational) ([]clock.Rational, *model.MediaError) {
	if len(reps) < 2 || len(reps) > 4 {
		return nil, model.New(model.StageSegment, "rep_count",
			fmt.Sprintf("need 2-4 representations, got %d", len(reps)), 1)
	}
	if segDur.Value <= 0 {
		return nil, model.New(model.StageSegment, "bad_duration",
			"segment duration must be positive", 1)
	}
	dur := commonDuration(reps)
	cuts := []clock.Rational{clock.New(0, 1)}
	for k := int64(1); ; k++ {
		t := clock.New(segDur.Value*k, segDur.Timescale)
		if t.Cmp(dur) >= 0 {
			break
		}
		if alignedEverywhere(reps, t) {
			cuts = append(cuts, t)
		}
	}
	if len(cuts) == 1 {
		return nil, model.New(model.StageSegment, "no_common_cut",
			"no grid point is a keyframe and audio boundary in every representation", 1)
	}
	cuts = append(cuts, dur)
	return cuts, nil
}

// commonDuration is the exact minimum of all video track durations.
func commonDuration(reps []*probe.Result) clock.Rational {
	var min clock.Rational
	for i, r := range reps {
		d := r.Track("video").Duration()
		if i == 0 || d.Cmp(min) < 0 {
			min = d
		}
	}
	return min
}

// alignedEverywhere checks t against every representation's keyframes
// and audio frame grid, using exact rational arithmetic only.
func alignedEverywhere(reps []*probe.Result, t clock.Rational) bool {
	for _, r := range reps {
		if !isKeyframe(r.Track("video"), t) {
			return false
		}
		if a := r.Track("audio"); a != nil && !onAudioGrid(a, t) {
			return false
		}
	}
	return true
}

func isKeyframe(v *probe.Track, t clock.Rational) bool {
	for _, s := range v.Samples {
		if s.Keyframe && clock.New(s.DTS, v.Timescale).Cmp(t) == 0 {
			return true
		}
	}
	return false
}

// onAudioGrid reports whether t is an exact multiple of the audio
// frame duration — the "sample boundary exactly on the cut" case must
// hold with zero tolerance.
func onAudioGrid(a *probe.Track, t clock.Rational) bool {
	if len(a.Samples) == 0 {
		return false
	}
	frame := a.Samples[0].Duration
	num := t.Value * a.Timescale
	if num%t.Timescale != 0 {
		return false
	}
	return (num/t.Timescale)%frame == 0
}
