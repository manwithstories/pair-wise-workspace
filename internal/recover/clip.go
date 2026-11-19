package recover

import (
	"archivearchivist/internal/clock"
	"archivearchivist/internal/model"
	"archivearchivist/internal/mp4"
)

// recoverClip snaps the clip's in and out points onto decodable keyframes
// using per-sample durations, so VFR material is never resampled to a constant
// frame rate.
func recoverClip(c model.Clip, in Input, cfg Config) []Boundary {
	sc := in.Scans[c.Track]
	if sc == nil || len(sc.Tracks) == 0 {
		return []Boundary{
			{Clip: c.ID, Track: c.Track, Edge: "in", Intended: c.Start, Status: "blocked", Keyframe: -1, ErrorUS: -1},
			{Clip: c.ID, Track: c.Track, Edge: "out", Intended: c.Start.Add(c.Duration), Status: "blocked", Keyframe: -1, ErrorUS: -1},
		}
	}
	tr := sc.Tracks[0]
	clk := in.Clocks[c.Track]
	mk := func(edge string, intended model.Rational, local model.Rational) Boundary {
		b := Boundary{Clip: c.ID, Track: c.Track, Edge: edge, Intended: intended, Keyframe: -1}
		idx, snapped, errUS := snapToKeyframe(tr, local, edge == "out")
		if idx < 0 {
			b.Status = "out_of_range"
			b.MediaTime = local
			b.ErrorUS = -1
			return b
		}
		s := tr.Samples[idx]
		mediaTicks := s.DTS
		// Convert sample ticks to a fraction of a second and apply the
		// track's measured drift so boundaries from different clocks compare.
		b.MediaTime = clock.ToSeconds(mediaTicks, tr.Timescale, clk.DriftPPM)
		b.Keyframe = int64(s.Index)
		b.MediaTicks = mediaTicks
		b.Snapped = snapped
		b.ErrorUS = errUS
		b.Status = "recovered"
		return b
	}
	return []Boundary{
		mk("in", c.Start, c.SourceIn),
		mk("out", c.Start.Add(c.Duration), c.SourceIn.Add(c.Duration)),
	}
}

// snapToKeyframe finds the sample whose media span contains want and returns
// the decodable keyframe at or before it. For an out-point (forward=true) the
// first keyframe at or after want is used instead, so the recovered clip still
// contains the intended span. The error is the distance in microseconds
// between want and the chosen keyframe.
func snapToKeyframe(tr mp4.Track, want model.Rational, forward bool) (idx int, snapped bool, errUS int64) {
	if len(tr.Samples) == 0 || tr.Timescale == 0 {
		return -1, false, -1
	}
	target := want.Scale(tr.Timescale)
	// The sample containing target, using each sample's own duration.
	var containing = -1
	for i, s := range tr.Samples {
		if s.Isolated {
			continue
		}
		next := s.DTS + s.Duration
		if target >= s.DTS && (target < next || i == len(tr.Samples)-1) {
			containing = i
			break
		}
	}
	if containing < 0 {
		return -1, false, -1
	}
	chosen := -1
	if forward {
		for i := containing; i < len(tr.Samples); i++ {
			if tr.Samples[i].Keyframe && !tr.Samples[i].Corrupt {
				chosen = i
				break
			}
		}
		// Past the last keyframe the out-point can only fall back, which
		// shortens the clip; the snap error makes that visible.
		if chosen < 0 {
			for i := containing; i >= 0; i-- {
				if tr.Samples[i].Keyframe && !tr.Samples[i].Corrupt {
					chosen = i
					break
				}
			}
		}
	} else {
		for i := containing; i >= 0; i-- {
			if tr.Samples[i].Keyframe && !tr.Samples[i].Corrupt {
				chosen = i
				break
			}
		}
		if chosen < 0 {
			for i := containing + 1; i < len(tr.Samples); i++ {
				if tr.Samples[i].Keyframe && !tr.Samples[i].Corrupt {
					chosen = i
					break
				}
			}
		}
	}
	if chosen < 0 {
		return -1, false, -1
	}
	delta := tr.Samples[chosen].DTS - target
	if delta < 0 {
		delta = -delta
	}
	return chosen, chosen != containing || tr.Samples[containing].DTS != target,
		delta * 1_000_000 / int64(tr.Timescale)
}

// recoverTransition compares the time bases of the two sides of a crossfade.
func recoverTransition(x model.Transition, bs []Boundary) TransitionResult {
	tr := TransitionResult{ID: x.ID, From: x.From, To: x.To, Status: "recovered"}
	find := func(clip, edge string) (Boundary, bool) {
		for _, b := range bs {
			if b.Clip == clip && b.Edge == edge && b.Status == "recovered" {
				return b, true
			}
		}
		return Boundary{}, false
	}
	a, okA := find(x.From, "out")
	b, okB := find(x.To, "in")
	if !okA || !okB {
		tr.Status = "blocked"
		tr.Detail = "one or both crossfade sides are not recovered"
		return tr
	}
	// The crossfade center is the same instant on both sides: the outgoing
	// clip's media position at its out-point must equal the incoming clip's
	// media position at its in-point, up to transport jitter.
	diff := a.MediaTime.Sub(b.MediaTime)
	tr.SkewUS = diff.Millis() * 1000
	if tr.SkewUS < 0 {
		tr.SkewUS = -tr.SkewUS
	}
	return tr
}
