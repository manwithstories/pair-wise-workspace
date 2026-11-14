package mp4atom

import (
	"mp4replay/internal/report"
)

const uint32Mod = int64(1) << 32

// Timeline is the fully normalized time mapping for one track.
type Timeline struct {
	// Media-axis values, in media timescale units, continuous int64.
	Dts        []int64
	Pts        []int64
	WrapEvents int
	// Presentation-axis values in milliseconds, after elst application.
	PresentMs  []int64
	EmptyEdits int
	// presentation length of the whole track in milliseconds
	DurationMs int64
}

// expandStts builds per-sample deltas and reconstructs a continuous DTS.
// A uint32-style rollover (the modular next value is smaller than the current
// one even though the delta is positive) is unwrapped by adding 2^32 and the
// event is counted. A negative delta is a genuine non-monotonic DTS and is
// reported rather than silently smoothed.
func expandStts(t *Table, n int) ([]int64, int, error) {
	dts := make([]int64, n)
	var cur int64
	wraps := 0
	idx := 0
	for _, e := range t.Stts {
		delta := int64(e.Delta)
		if delta < 0 {
			return nil, 0, report.NewError(report.ExitBoxInvalid, "timeline",
				"non-monotonic DTS: negative stts delta")
		}
		for k := uint32(0); k < e.Count; k++ {
			if idx >= n {
				return nil, 0, report.NewError(report.ExitBoxInvalid, "timeline",
					"stts covers more samples than stsc/stsz")
			}
			if idx > 0 {
				next := cur + delta
				if uint32(next) < uint32(cur) { // modular uint32 rollover
					next += uint32Mod
					wraps++
				}
				cur = next
			}
			dts[idx] = cur
			idx++
		}
	}
	if idx != n {
		return nil, 0, report.NewError(report.ExitBoxInvalid, "timeline",
			"stts/stsc sample count mismatch")
	}
	return dts, wraps, nil
}

// expandCtts returns a signed per-sample composition offset, version aware.
func expandCtts(t *Table, n int) ([]int64, error) {
	off := make([]int64, n)
	if !t.HasCtts {
		return off, nil
	}
	idx := 0
	for _, e := range t.Ctts {
		for k := uint32(0); k < e.Count; k++ {
			if idx >= n {
				return nil, report.NewError(report.ExitBoxInvalid, "timeline",
					"ctts covers more samples than stsc/stsz")
			}
			off[idx] = e.Offset
			idx++
		}
	}
	if idx != n {
		return nil, report.NewError(report.ExitBoxInvalid, "timeline",
			"ctts/stsc sample count mismatch")
	}
	return off, nil
}

// BuildTimeline produces DTS/PTS plus the edit-list adjusted presentation axis.
// movieTS is the mvhd timescale used by elst segment durations.
func BuildTimeline(t *Table, movieTS uint32, samples []Sample) (*Timeline, error) {
	t.MovieTS = movieTS
	n := len(samples)
	dts, wraps, err := expandStts(t, n)
	if err != nil {
		return nil, err
	}
	coff, err := expandCtts(t, n)
	if err != nil {
		return nil, err
	}
	tl := &Timeline{Dts: dts, WrapEvents: wraps}
	tl.Pts = make([]int64, n)
	for i := range samples {
		tl.Pts[i] = dts[i] + coff[i]
		samples[i].Dts = dts[i]
		samples[i].CttsOff = coff[i]
		samples[i].Pts = tl.Pts[i]
	}
	if err := applyEdits(t, samples, tl); err != nil {
		return nil, err
	}
	return tl, nil
}

// scaleMs converts media-timescale units into milliseconds, rounding to nearest.
func scaleMs(v int64, timescale uint32) int64 {
	if timescale == 0 {
		return 0
	}
	if v >= 0 {
		return (v*1000 + int64(timescale)/2) / int64(timescale)
	}
	return -(((-v)*1000 + int64(timescale)/2) / int64(timescale))
}
