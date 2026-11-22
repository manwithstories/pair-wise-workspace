// Package repair turns time-anchored faults into adjudicated segments.
// Recoverable ordering defects are kept; structural damage is isolated
// into quarantined segments while every later valid segment remains
// processable. Decisions are deterministic and never hide a gap.
package repair

import (
	"sort"

	"mediacheck/internal/model"
)

// Policy controls segment merging and adjudication.
type Policy struct {
	// MergeGapMs merges faults of the same track whose gap is at most
	// this many milliseconds into one segment.
	MergeGapMs int64
	// MaxQuarantineRatio bounds the share of timeline that may be
	// isolated; beyond it the run is flagged unrecovered instead.
	MaxQuarantineRatio float64
}

// DefaultPolicy matches the broadcast acceptance posture.
func DefaultPolicy() Policy {
	return Policy{MergeGapMs: 250, MaxQuarantineRatio: 0.5}
}

// Result is the adjudication output.
type Result struct {
	Segments   []model.Segment
	Faults     []model.Fault // faults with final recovery state
	Boundaries []model.CommittedBoundary
}

// Adjudicate groups faults into segments per track, merges nearby
// spans, assigns a recovery state and computes committed boundaries.
func Adjudicate(faults []model.Fault, trackEnds map[string]int64, pol Policy) Result {
	// Stable copy: sort by track, then time, then ID for determinism.
	sorted := make([]model.Fault, len(faults))
	copy(sorted, faults)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Track != b.Track {
			return a.Track < b.Track
		}
		if a.StartMs != b.StartMs {
			return a.StartMs < b.StartMs
		}
		return a.ID < b.ID
	})

	res := Result{
		Segments:   []model.Segment{},
		Faults:     sorted,
		Boundaries: []model.CommittedBoundary{},
	}
	nextSegID := 1
	byTrack := map[string][]model.Fault{}
	for _, f := range sorted {
		byTrack[f.Track] = append(byTrack[f.Track], f)
	}
	// Deterministic track iteration order.
	tracks := make([]string, 0, len(byTrack))
	for tr := range byTrack {
		tracks = append(tracks, tr)
	}
	sort.Strings(tracks)

	totalLen := map[string]int64{}
	quarantined := map[string]int64{}
	for _, tr := range tracks {
		fs := byTrack[tr]
		var cur *model.Segment
		flush := func() {
			if cur == nil {
				return
			}
			cur.ID = nextSegID
			nextSegID++
			res.Segments = append(res.Segments, *cur)
			if cur.Recovery == model.RecQuarantined {
				quarantined[tr] += cur.EndMs - cur.StartMs
			}
			cur = nil
		}
		for _, f := range fs {
			rec := decide(f)
			// Apply recovery state back onto the fault.
			for i := range res.Faults {
				if res.Faults[i].ID == f.ID {
					res.Faults[i].Recovery = rec
				}
			}
			if cur != nil && f.StartMs <= cur.EndMs+pol.MergeGapMs &&
				(rec == model.RecQuarantined) == (cur.Recovery == model.RecQuarantined) {
				cur.EndMs = max64(cur.EndMs, f.EndMs)
				cur.FaultIDs = append(cur.FaultIDs, f.ID)
				continue
			}
			flush()
			cur = &model.Segment{
				Track: tr, StartMs: f.StartMs, EndMs: max64(f.EndMs, f.StartMs+1),
				FaultIDs: []int{f.ID}, Recovery: rec, Reason: f.Kind,
			}
		}
		flush()
		totalLen[tr] = trackEnds[tr]
	}

	// Enforce quarantine budget: an over-budget track ends unrecovered.
	for i := range res.Segments {
		seg := &res.Segments[i]
		end := totalLen[seg.Track]
		if end > 0 && pol.MaxQuarantineRatio > 0 &&
			float64(quarantined[seg.Track])/float64(end) > pol.MaxQuarantineRatio &&
			seg.Recovery == model.RecQuarantined {
			seg.Recovery = model.RecUnrecovered
			for j := range res.Faults {
				if res.Faults[j].Track == seg.Track &&
					res.Faults[j].StartMs >= seg.StartMs && res.Faults[j].StartMs < seg.EndMs {
					res.Faults[j].Recovery = model.RecUnrecovered
				}
			}
		}
	}

	res.Boundaries = committedBoundaries(res.Segments, tracks, trackEnds)
	return res
}

// decide maps a single fault to its recovery state. A fault already
// marked recovered by its source (e.g. a reordered RTP gap) is kept;
// every structural defect and every unfilled hole is quarantined
// rather than silently repaired.
func decide(f model.Fault) model.Recovery {
	if f.Recovery == model.RecRecovered {
		return model.RecRecovered
	}
	return model.RecQuarantined
}

// committedBoundaries reports, per track, the last safe end before each
// quarantined span and the final end, so the sink knows exactly how
// much media was durably committed.
func committedBoundaries(segs []model.Segment, tracks []string, ends map[string]int64) []model.CommittedBoundary {
	out := []model.CommittedBoundary{}
	for _, tr := range tracks {
		var last int64
		segID := 0
		for i := range segs {
			s := &segs[i]
			if s.Track != tr {
				continue
			}
			if s.Recovery == model.RecQuarantined {
				out = append(out, model.CommittedBoundary{Track: tr, EndMs: last, Segment: segID})
				last = s.EndMs // skip the hole; resume after it
				segID = s.ID
				continue
			}
			last = max64(last, s.EndMs)
			segID = s.ID
		}
		out = append(out, model.CommittedBoundary{Track: tr, EndMs: ends[tr], Segment: segID})
	}
	return out
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// HasUnrecovered reports whether any segment remains unresolved.
func (r Result) HasUnrecovered() bool {
	for _, s := range r.Segments {
		if s.Recovery == model.RecUnrecovered {
			return true
		}
	}
	return false
}
