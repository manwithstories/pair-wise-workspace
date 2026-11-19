package pipeline

import (
	"archivearchivist/internal/model"
	"archivearchivist/internal/report"
)

// summarize derives the acceptance metrics from the analysis.
func summarize(a *Analysis) report.Metrics {
	m := report.Metrics{Tracks: len(a.Timeline.Tracks), ConstantFPS: true}
	m.CorruptRanges = len(a.Plan.Ranges)
	for _, r := range a.Plan.Ranges {
		if r.Status == "recovered" {
			m.RecoveredRanges++
		} else {
			m.UnrecoverableRanges++
		}
	}
	for _, cv := range a.Clocks {
		m.Wraps += cv.Wraps
	}
	for _, st := range a.Plan.Tracks {
		if st.VFR {
			m.VFRTracks++
			m.ConstantFPS = false
		}
		if st.Keyframes == 0 && st.OK {
			m.ConstantFPS = false
		}
	}
	var snapErrs []int64
	for _, b := range a.Plan.Boundaries {
		if b.Status == "recovered" {
			m.RecoveredBoundaries++
			snapErrs = append(snapErrs, b.ErrorUS)
		} else {
			m.BlockedBoundaries++
		}
	}
	m.SnapErrorP95US = report.Quantile(snapErrs, 95)
	m.SnapErrorMaxUS = report.Quantile(snapErrs, 100)

	// End-to-end synchronization error is the transport clock's disagreement
	// with media time after unwrap and drift correction, sampled per packet.
	syncErrs := a.SyncSamplesUS()
	m.SyncSamples = len(syncErrs)
	m.SyncErrorP95US = report.Quantile(syncErrs, 95)
	m.SyncErrorMaxUS = report.Quantile(syncErrs, 100)

	var xs []int64
	for _, tr := range a.Plan.Transitions {
		if tr.Status == "recovered" {
			xs = append(xs, tr.SkewUS)
		}
	}
	m.CrossfadeSkewP95US = report.Quantile(xs, 95)
	if a.Capture != nil {
		m.PacketRecoveryRate = a.Capture.RecoveryRate()
		m.RecoveredPackets = a.Capture.Accepted
		// Late packets lie inside the observed sequence span, so they are
		// already part of Missing; the gap must not count them twice.
		m.TotalGap = a.Capture.Missing
		m.LatePackets = a.Capture.Late
	} else {
		m.PacketRecoveryRate = model.NewRat(1, 1)
	}
	for _, cv := range a.Clocks {
		if cv.DriftReview {
			m.DriftReviewTracks = append(m.DriftReviewTracks, cv.Track)
		}
	}
	return m
}

// SyncSamplesUS returns the absolute skew of every packet in the capture
// against the drift-corrected media clock. These are the end-to-end
// synchronization samples the acceptance run reports.
func (a *Analysis) SyncSamplesUS() []int64 {
	out := make([]int64, 0, 256)
	for _, res := range a.clockResults {
		for _, p := range res.Points {
			if p.HasArrival {
				out = append(out, report.Abs(p.SkewUS))
			}
		}
	}
	return out
}

func captureTrack(tl *model.Timeline) string {
	for _, t := range tl.Tracks {
		if t.ClockModulus > 0 {
			return t.ID
		}
	}
	if len(tl.Tracks) > 0 {
		return tl.Tracks[0].ID
	}
	return ""
}
