package segment

import (
	"mediacheck/internal/clock"
	"mediacheck/internal/probe"
)

// AudioAlignment measures how closely an audio track can meet a shared cut.
// Audio sample boundaries rarely coincide exactly with video cuts; the
// bounded fallback snaps to the nearest sample and reports the residual.
type AudioAlignment struct {
	CutIndex      int
	RequestedAt   int64
	SnappedAt     int64
	ResidualTicks int64
	SampleIndex   uint32
}

// ResidualMillis is the absolute audio/video boundary mismatch in ms.
func (a AudioAlignment) ResidualMillis() int64 {
	r := a.ResidualTicks
	if r < 0 {
		r = -r
	}
	return clock.TicksToMilliseconds(r)
}

// AlignAudioTrack snaps each shared cut to the nearest audio sample boundary
// within maxDrift. Cuts that cannot be matched keep their ideal position and
// the residual is returned so the manifest records the A/V boundary error.
func AlignAudioTrack(tr *probe.Track, cuts []Cut, maxDrift int64) ([]Cut, []AudioAlignment) {
	if !tr.IsVideo && len(tr.Samples) == 0 {
		return cuts, nil
	}
	out := make([]Cut, len(cuts))
	copy(out, cuts)
	var aligns []AudioAlignment
	for i := range out {
		idx := nearestSample(tr, out[i].IdealAt)
		if idx < 0 {
			continue
		}
		snapped := tr.Samples[idx].StartTicks
		residual := snapped - out[i].IdealAt
		abs := residual
		if abs < 0 {
			abs = -abs
		}
		aa := AudioAlignment{
			CutIndex: i, RequestedAt: out[i].IdealAt,
			SnappedAt: snapped, ResidualTicks: residual, SampleIndex: uint32(idx),
		}
		if abs <= maxDrift {
			// Bounded fallback: accept the nearest audio sample boundary.
			out[i].ChosenAt = snapped
			out[i].SampleIdx = uint32(idx)
		}
		aligns = append(aligns, aa)
	}
	return out, aligns
}

// nearestSample returns the index of the sample whose start is closest to
// at, preferring the earlier sample on an exact tie.
func nearestSample(tr *probe.Track, at int64) int {
	lo, hi := 0, len(tr.Samples)
	for lo < hi {
		mid := (lo + hi) / 2
		if tr.Samples[mid].StartTicks < at {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == len(tr.Samples) {
		return lo - 1
	}
	if lo == 0 {
		return 0
	}
	prev := tr.Samples[lo-1].StartTicks
	cur := tr.Samples[lo].StartTicks
	if at-prev <= cur-at {
		return lo - 1
	}
	return lo
}

// MaxAVSyncMillis scans audio alignments and returns the largest residual,
// feeding the demo's av_sync_max_ms acceptance field.
func MaxAVSyncMillis(aligns []AudioAlignment) int64 {
	var max int64
	for _, a := range aligns {
		if ms := a.ResidualMillis(); ms > max {
			max = ms
		}
	}
	return max
}
