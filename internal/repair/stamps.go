// Package repair fixes regressive or missing timestamps while keeping
// decode order and presentation-duration invariants. When a timestamp
// cannot be inferred it marks a gap and stops the track.
package repair

import (
	"mediaqa/internal/media"
	mp4scan "mediaqa/internal/mp4"
)

// MissingDTS is the sentinel for a sample whose timestamp is absent.
const MissingDTS = int64(-1) << 62

// Stamps processes samples in decode order. Corrupt samples are dropped
// (already registered by the scanner). It returns the repaired prefix;
// when inference is impossible it registers a fatal gap and stops.
func Stamps(samples []mp4scan.Sample, timescale int64, trackID string, errs *media.List) []mp4scan.Sample {
	ts := timescale
	if ts <= 0 {
		ts = 1
	}
	out := make([]mp4scan.Sample, 0, len(samples))
	var prevDTS, prevDur int64
	started := false
	for _, s := range samples {
		if s.Corrupt {
			continue
		}
		dts := s.DTS
		repaired := false
		switch {
		case dts == MissingDTS:
			if !started || prevDur <= 0 {
				errs.Addf(media.SeverityFatal, trackID, "timestamp-gap",
					media.Rational{}, media.Rational{},
					"sample %d: missing timestamp with no prior frame to infer from", s.Index)
				return out
			}
			dts = prevDTS + prevDur
			repaired = true
		case started && dts <= prevDTS:
			if prevDur <= 0 {
				errs.Addf(media.SeverityFatal, trackID, "timestamp-gap",
					media.FromTicks(prevDTS, ts), media.FromTicks(dts, ts),
					"sample %d: regressive DTS %d and zero prior duration; cannot infer", s.Index, dts)
				return out
			}
			dts = prevDTS + prevDur
			repaired = true
		}
		if repaired {
			// Preserve the composition offset so presentation
			// durations are unchanged.
			offset := s.PTS - s.DTS
			errs.Addf(media.SeverityRecoverable, trackID, "repaired-stamp",
				media.FromTicks(dts, ts), media.FromTicks(dts+s.Duration, ts),
				"sample %d: DTS reassigned to %d", s.Index, dts)
			s.DTS = dts
			s.PTS = dts + offset
		}
		out = append(out, s)
		prevDTS, prevDur = dts, s.Duration
		started = true
	}
	return out
}
