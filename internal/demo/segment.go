package demo

import (
	"fmt"

	"mediascan/internal/fault"
)

// scanSegment produces the deterministic findings for frames in [start,end).
// Faults are pure functions of (seed, track, frame), so any segmentation and
// any resume boundary reconstructs identical per-frame results. Four fault
// families are exercised, with truncation and keyframe/drift spanning the
// whole scan -> merge -> recovery chain.
func (e *Engine) scanSegment(start, end int64) []fault.Fault {
	var out []fault.Fault
	s := e.spec
	for frame := start; frame < end; frame++ {
		for track := uint32(0); track < uint32(s.Tracks); track++ {
			// 1) Frame-localized truncation/corruption on track 0.
			if track == 0 && s.CorruptAt >= 0 && frame == s.CorruptAt {
				off := FrameOffset(e.media, int(track), int(frame))
				out = append(out, fault.New(
					fault.KindTruncatedBox, fault.SeverityFatal, track+1,
					frame, off, e.frameTicks(track, frame),
					fmt.Sprintf("corrupt frame: box payload unreadable at byte %d", off),
				).WithSpan(1, 16))
			}
			// 2) Keyframe interval violations on video tracks (tracks 0,2).
			if s.KeyEvery > 0 && (track == 0 || track == 2) {
				if gap := e.keyframeGap(track, frame); gap > s.KeyGapLimit {
					out = append(out, fault.New(
						fault.KindKeyframeGap, fault.SeverityWarning, track+1,
						frame, FrameOffset(e.media, int(track), int(frame)),
						e.frameTicks(track, frame),
						fmt.Sprintf("keyframe gap %d ticks exceeds %d", gap, s.KeyGapLimit),
					))
				}
			}
			// 3) Clock drift on metadata track (last track) above threshold.
			if int(track) == s.Tracks-1 {
				dev := e.driftDeviation(frame)
				if abs(dev) > s.DriftLimit {
					dir := fault.KindClockDrift
					_ = dir
					out = append(out, fault.New(
						fault.KindClockDrift, fault.SeverityWarning, track+1,
						frame, -1, e.frameTicks(track, frame),
						fmt.Sprintf("clock drift deviation=%d ticks limit=%d", dev, s.DriftLimit),
					))
				}
			}
			// 4) RTP sequence gap on a fixed cadence (track 1), exercising
			// the RTP subsystem's localization through the same pipeline.
			if track == 1 && frame > 0 && frame%97 == 0 {
				out = append(out, fault.New(
					fault.KindRTPSeqGap, fault.SeverityError, track+1,
					frame, -1, e.frameTicks(track, frame),
					fmt.Sprintf("missing RTP sequence at frame %d", frame),
				).WithSpan(1, 0))
			}
		}
	}
	return out
}

// frameTicks computes a frame's start on the unified grid using the same
// alternating VFR durations the fixture encodes, via exact integer math.
func (e *Engine) frameTicks(track uint32, frame int64) int64 {
	s := e.spec
	// Two-cycle timescale deltas matching fixture VFRDurations.
	d0 := s.Timescale / 30
	d1 := s.Timescale / 24
	var units int64
	for f := int64(0); f < frame; f++ {
		if f%2 == 0 {
			units += int64(d0)
		} else {
			units += int64(d1)
		}
	}
	// Exact rational conversion units * 1e6 / timescale.
	return units * 1_000_000 / s.Timescale
}

// keyframeGap returns the presentation-time distance since the last keyframe
// for the given frame, or 0 when the frame itself is a keyframe.
func (e *Engine) keyframeGap(track uint32, frame int64) int64 {
	if e.spec.KeyEvery <= 0 || frame%int64(e.spec.KeyEvery) == 0 {
		return 0
	}
	last := frame - (frame % int64(e.spec.KeyEvery))
	return e.frameTicks(track, frame) - e.frameTicks(track, last)
}

// driftDeviation is a deterministic cumulative drift on the metadata track:
// a slow linear drift plus a seed-dependent periodic component, kept exact in
// integer ticks so VFR and drift compose without float error.
func (e *Engine) driftDeviation(frame int64) int64 {
	// 120 ticks per frame of linear drift, plus a bounded oscillation.
	linear := frame * 120
	osc := int64(((frame*7 + e.spec.Seed) % 200) - 100)
	return linear + osc
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
