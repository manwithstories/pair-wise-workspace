// Package align computes switchable segment boundaries across several
// representations (bitrates) of the same material. A boundary is switchable
// only when every representation has a keyframe at or before it; intervals
// where a representation lacks a keyframe are reported as non-switchable gaps
// instead of being dropped silently.
package align

import (
	"sort"

	"medialine/internal/fault"
	"medialine/internal/index"
)

// Representation is one bitrate ladder rung: its identity plus the track that
// carries it on the unified timeline.
type Representation struct {
	ID      string
	Bitrate int64
	TrackID int
	Index   *index.TrackIndex
}

// Config controls boundary selection.
type Config struct {
	// SegmentUS is the nominal segment length.
	SegmentUS int64
	// SnapBack snaps a requested boundary to the nearest earlier keyframe.
	// When false, a boundary is only accepted if every representation has a
	// keyframe exactly at it, and otherwise the interval is marked non-switchable.
	SnapBack bool
	// RequireAll demands a common boundary across all representations. When
	// false, a boundary missing some representation yields a gap for those
	// representations only.
	RequireAll bool
	// EditPoints are operator cuts that must become boundaries in addition to
	// the nominal segment grid. They are snapped like any other boundary.
	EditPoints []int64
}

// Boundary is one aligned cut point.
type Boundary struct {
	RequestedUS int64
	EffectiveUS int64
	// Switchable is true when every representation has a keyframe here.
	Switchable bool
	// Missing lists representation IDs without a keyframe at EffectiveUS.
	Missing []string
}

// Aligned is the complete boundary list for a ladder.
type Aligned struct {
	Boundaries []Boundary
	// Gaps holds the non-switchable intervals, by representation.
	Gaps map[string][]Interval
	// SpanUS is the timeline length used for the last boundary.
	SpanUS int64
}

// Interval is a half-open microsecond range.
type Interval struct {
	StartUS int64
	EndUS   int64
}

// Align computes all boundaries for reps over [0, spanUS). It returns a fault
// when no representation is usable; otherwise gaps are returned in-band so
// callers can still emit the switchable fragments.
func Align(reps []Representation, spanUS int64, cfg Config) (*Aligned, error) {
	if len(reps) == 0 {
		return nil, fault.New(fault.StageAlign, fault.KindBadConfig, "no representations to align")
	}
	if cfg.SegmentUS <= 0 {
		return nil, fault.New(fault.StageAlign, fault.KindBadConfig,
			"segment length %dus must be positive", cfg.SegmentUS)
	}
	if spanUS <= 0 {
		return nil, fault.New(fault.StageAlign, fault.KindZeroDuration, "timeline span is zero")
	}
	ordered := append([]Representation(nil), reps...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	for _, r := range ordered {
		if r.Index == nil {
			return nil, fault.New(fault.StageAlign, fault.KindBadConfig,
				"representation %s has no keyframe index", r.ID)
		}
	}

	var requests []int64
	for t := int64(0); t < spanUS; t += cfg.SegmentUS {
		requests = append(requests, t)
	}
	if len(requests) == 0 {
		requests = []int64{0}
	}
	for _, at := range cfg.EditPoints {
		if at <= 0 || at >= spanUS {
			return nil, fault.New(fault.StageAlign, fault.KindBadConfig,
				"edit point %dus lies outside (0,%d)", at, spanUS).At(fault.UnknownTrack, at)
		}
		requests = append(requests, at)
	}
	sort.SliceStable(requests, func(i, j int) bool { return requests[i] < requests[j] })
	uniq := requests[:0]
	for i, r := range requests {
		if i > 0 && requests[i-1] == r {
			continue
		}
		uniq = append(uniq, r)
	}
	requests = append(uniq, spanUS)

	out := &Aligned{SpanUS: spanUS, Gaps: make(map[string][]Interval)}
	prevEffective := int64(-1)
	for i, req := range requests {
		if i == len(requests)-1 && req == spanUS {
			// The end of the timeline closes the final segment; it does not
			// need a keyframe and is never reported as a gap.
			out.Boundaries = append(out.Boundaries, Boundary{RequestedUS: req, EffectiveUS: req, Switchable: true})
			continue
		}
		b := Boundary{RequestedUS: req, EffectiveUS: req, Switchable: true}
		for _, r := range ordered {
			if cfg.SnapBack {
				sr := r.Index.SnapBack(req)
				if !sr.Found {
					b.Switchable = false
					b.Missing = append(b.Missing, r.ID)
					continue
				}
				b.EffectiveUS = sr.SnapUS
				continue
			}
			if _, ok := r.Index.Exact(req); !ok {
				b.Switchable = false
				b.Missing = append(b.Missing, r.ID)
			}
		}
		if b.EffectiveUS <= prevEffective {
			// Snapping pulled this boundary onto an earlier one; drop the
			// duplicate so segment IDs stay unique.
			continue
		}
		prevEffective = b.EffectiveUS
		out.Boundaries = append(out.Boundaries, b)
	}
	// Remove any boundary whose effective time duplicates the timeline end.
	dedup := out.Boundaries[:0]
	for i, b := range out.Boundaries {
		if i+1 < len(out.Boundaries) && b.EffectiveUS == out.Boundaries[i+1].EffectiveUS {
			continue
		}
		dedup = append(dedup, b)
	}
	out.Boundaries = dedup

	for i := 0; i+1 < len(out.Boundaries); i++ {
		from, to := out.Boundaries[i], out.Boundaries[i+1]
		if from.EffectiveUS >= to.EffectiveUS {
			continue
		}
		interval := Interval{StartUS: from.EffectiveUS, EndUS: to.EffectiveUS}
		for _, r := range ordered {
			if usable(r, interval) {
				continue
			}
			out.Gaps[r.ID] = append(out.Gaps[r.ID], interval)
			from.Switchable = false
			from.Missing = appendUnique(from.Missing, r.ID)
		}
		out.Boundaries[i] = from
	}
	for id := range out.Gaps {
		sort.Slice(out.Gaps[id], func(i, j int) bool { return out.Gaps[id][i].StartUS < out.Gaps[id][j].StartUS })
	}
	if cfg.RequireAll {
		for _, b := range out.Boundaries {
			if len(b.Missing) > 0 && b.EffectiveUS != spanUS {
				// Callers asking for a uniform ladder get an explicit fault
				// naming the first unusable boundary.
				return out, fault.New(fault.StageAlign, fault.KindMissingIndex,
					"boundary %dus is not switchable across all representations", b.EffectiveUS).
					At(fault.UnknownTrack, b.EffectiveUS)
			}
		}
	}
	return out, nil
}

// usable reports whether rep covers the whole interval with keyframe-anchored
// samples, i.e. it has a keyframe at the interval start and at least one
// sample after it.
func usable(r Representation, in Interval) bool {
	if _, ok := r.Index.Exact(in.StartUS); !ok {
		// A representation may start its own content at or before the
		// boundary only if a keyframe lands exactly on it.
		return false
	}
	kf, ok := r.Index.After(in.StartUS)
	if !ok {
		return false
	}
	return kf.PTSUS <= in.EndUS || kf.PTSUS == in.StartUS
}

func appendUnique(list []string, v string) []string {
	for _, s := range list {
		if s == v {
			return list
		}
	}
	return append(list, v)
}

// MaxGap reports the widest non-switchable interval across all
// representations, in microseconds. Zero means every boundary is switchable.
func (a *Aligned) MaxGap() int64 {
	var max int64
	for _, gaps := range a.Gaps {
		for _, g := range gaps {
			if d := g.EndUS - g.StartUS; d > max {
				max = d
			}
		}
	}
	return max
}

// BoundaryDelta returns the difference between the maximum and minimum
// effective boundary across representations. Because Align already forces a
// common effective time, this is zero for fully switchable boundaries and
// reports the residual spread for non-switchable ones.
func (a *Aligned) BoundaryDelta(repIDs []string, indexes map[string]*index.TrackIndex) int64 {
	var maxDelta int64
	for _, b := range a.Boundaries {
		var lo, hi int64
		first := true
		for _, id := range repIDs {
			idx, ok := indexes[id]
			if !ok {
				continue
			}
			kf, ok := idx.Before(b.EffectiveUS)
			if !ok {
				continue
			}
			if first {
				lo, hi = kf.PTSUS, kf.PTSUS
				first = false
				continue
			}
			if kf.PTSUS < lo {
				lo = kf.PTSUS
			}
			if kf.PTSUS > hi {
				hi = kf.PTSUS
			}
		}
		if d := hi - lo; d > maxDelta {
			maxDelta = d
		}
	}
	return maxDelta
}
