package pipeline

import (
	"pairflow/media/internal/clock"
	"pairflow/media/internal/mp4"
)

// SyncResult compares the node's RTP timeline against the origin's MP4
// decode timeline before and after the fitted offset/drift correction.
// Both sides are expressed in the same 90 kHz rational basis; the raw
// residual shows the uncorrected skew and the corrected residual shows
// what the shared clock model recovered.
type SyncResult struct {
	Compared         int            `json:"compared"`
	RawMaxMS         clock.Rational `json:"raw_max_residual_ms"`
	CorrectedMaxMS   clock.Rational `json:"corrected_max_residual_ms"`
	CorrectedMeanMS  clock.Rational `json:"corrected_mean_residual_ms"`
	CleanMaxMS       clock.Rational `json:"clean_max_residual_ms"`
	AnomalyMaxMS     clock.Rational `json:"anomaly_max_residual_ms"`
	ImprovementRatio clock.Rational `json:"improvement_ratio"`
}

// syncRTPToMP4 pairs each unwrapped RTP packet with the MP4 sample at
// the same ordinal, which is valid because both fixtures put one packet
// per video frame. It never rewrites a timestamp: the corrected value is
// reported alongside the raw one.
func syncRTPToMP4(pairs []clock.Pair, track mp4.TrackIndex, corr clock.Correction, base clock.Base, anomalies []clock.Anomaly) SyncResult {
	var out SyncResult
	n := len(pairs)
	if len(track.Samples) < n {
		n = len(track.Samples)
	}
	if n == 0 {
		return out
	}
	var rawMax, correctedMax, correctedSum, cleanMax, anomalyMax clock.Rational
	isAnomaly := make(map[int]bool, len(anomalies))
	for _, a := range anomalies {
		isAnomaly[a.Index] = true
	}
	origin := pairs[0]
	for i := 0; i < n; i++ {
		pts := track.Samples[i].DTS
		rawSec := base.Time(pairs[i].NodeTicks - origin.NodeTicks)
		raw := rawSec.Sub(pts)
		if absRat(raw).Cmp(absRat(rawMax)) > 0 {
			rawMax = raw
		}
		corrected := corr.Correct(pairs[i].NodeTicks-origin.NodeTicks, base)
		cres := corrected.Sub(pts)
		if absRat(cres).Cmp(absRat(correctedMax)) > 0 {
			correctedMax = cres
		}
		if isAnomaly[i] {
			if absRat(cres).Cmp(absRat(anomalyMax)) > 0 {
				anomalyMax = cres
			}
		} else if absRat(cres).Cmp(absRat(cleanMax)) > 0 {
			cleanMax = cres
		}
		correctedSum = correctedSum.Add(absRat(cres))
		out.Compared++
	}
	out.RawMaxMS = absRat(rawMax).Mul(clock.NewRational(1000, 1))
	out.CorrectedMaxMS = absRat(correctedMax).Mul(clock.NewRational(1000, 1))
	out.CorrectedMeanMS = correctedSum.Div(clock.NewRational(int64(n), 1)).Mul(clock.NewRational(1000, 1))
	out.CleanMaxMS = absRat(cleanMax).Mul(clock.NewRational(1000, 1))
	out.AnomalyMaxMS = absRat(anomalyMax).Mul(clock.NewRational(1000, 1))
	if !out.CorrectedMaxMS.IsZero() {
		out.ImprovementRatio = out.RawMaxMS.Div(out.CorrectedMaxMS)
	}
	return out
}

func absRat(r clock.Rational) clock.Rational {
	if r.Num < 0 {
		return r.Neg()
	}
	return r
}
