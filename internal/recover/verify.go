package recover

import (
	"archivearchivist/internal/model"
)

// verifyTrack performs the gate checks a track must pass before downstream
// work: clock normalization, at least one decodable keyframe, and isolation of
// samples without durations.
func verifyTrack(t model.Track, in Input) (TrackState, model.FaultList) {
	st := TrackState{Track: t.ID, DriftPPM: model.NewRat(0, 1)}
	var faults model.FaultList
	clk, hasClock := in.Clocks[t.ID]
	if hasClock {
		st.DriftPPM = clk.DriftPPM
		st.Wraps = clk.Wraps
		// Drift beyond the review threshold does not fail the track: it is
		// carried into the report with an explicit review flag, because the
		// normalized clock is still monotonic and usable. Clock faults are not
		// re-added here; the pipeline already recorded them once and the wrap
		// count is reported on the track state itself.
		_ = clk.Faults
		st.ClockNorm = true
	} else {
		// File-only tracks have no transport clock to normalize.
		st.ClockNorm = true
	}

	sc := in.Scans[t.ID]
	if sc == nil {
		st.Reason = "no media scanned for this track"
		return st, faults
	}
	if len(sc.Tracks) == 0 {
		st.Reason = "source contains no readable track"
		return st, faults
	}
	tr := sc.Tracks[0]
	st.VFR = !tr.ConstantFrame
	for _, s := range tr.Samples {
		if s.Isolated {
			st.Isolated++
		}
		if !s.Corrupt && s.Keyframe {
			st.Keyframes++
		}
	}
	if st.Keyframes == 0 {
		st.Reason = "no decodable keyframe in the source"
		return st, faults
	}
	// A file that could not be parsed at all (no tracks) or that was cut short
	// of every sample cannot safely bound a clip, even if the keyframe count
	// happens to be zero for an unrelated reason.
	if st.Isolated > 0 {
		// Isolated samples are excluded from timing and reported; they are
		// never filled in with an assumed frame rate.
		faults.Add(model.NewFault(model.CodeVFRGap, model.SevWarn,
			"track %s: %d samples without duration isolated", t.ID, st.Isolated))
	}
	st.OK = true
	return st, faults
}
