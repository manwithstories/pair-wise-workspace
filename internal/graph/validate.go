package graph

import (
	"sort"

	"mediasim/internal/clock"
)

func validateSegment(seg *Segment, seen map[string]Segment) error {
	if seg.ID == "" {
		return invalid(clock.CatTimeline, span(*seg), "segment with empty id")
	}
	if _, dup := seen[seg.ID]; dup {
		return invalid(clock.CatTimeline, span(*seg), "duplicate segment id")
	}
	seen[seg.ID] = *seg
	if seg.Start < 0 {
		return invalid(clock.CatTimeline, span(*seg), "negative start %d", seg.Start)
	}
	if seg.Duration < 0 {
		return invalid(clock.CatTimeline, span(*seg), "negative duration %d", seg.Duration)
	}
	if _, err := clock.AddTicks(seg.Start, seg.Duration); err != nil {
		return invalid(clock.CatTimeline, span(*seg), "segment end overflows: %v", err)
	}
	if seg.Source == "" {
		return invalid(clock.CatTimeline, span(*seg), "segment with empty source")
	}
	if len(seg.EditPoints) > MaxEditPoints {
		return invalid(clock.CatTimeline, span(*seg), "edit points %d exceed %d", len(seg.EditPoints), MaxEditPoints)
	}
	prev := int64(0)
	for _, p := range seg.EditPoints {
		if p <= prev || p >= seg.Duration {
			return invalid(clock.CatTimeline, span(*seg),
				"edit point %d not strictly inside (0,%d)", p, seg.Duration)
		}
		prev = p
	}
	return nil
}

// validateTrackOverlap rejects implicit overlap. Overlap is legal only when a
// transition explicitly declares exactly that many ticks between the two
// adjacent segments.
func (t *Timeline) validateTrackOverlap(tr Track) error {
	segs := append([]Segment(nil), tr.Segments...)
	sort.SliceStable(segs, func(i, j int) bool {
		if segs[i].Start != segs[j].Start {
			return segs[i].Start < segs[j].Start
		}
		return segs[i].ID < segs[j].ID
	})
	for i := 1; i < len(segs); i++ {
		a, b := segs[i-1], segs[i]
		if b.Start >= a.Start+a.Duration {
			continue
		}
		overlap := a.Start + a.Duration - b.Start
		if !t.overlapDeclared(a.ID, b.ID, overlap) {
			return invalid(clock.CatTimeline, span(b), "implicit overlap of %d ticks with %s", overlap, a.ID)
		}
	}
	return nil
}

func (t *Timeline) overlapDeclared(from, to string, ticks int64) bool {
	for _, tr := range t.Transitions {
		if tr.From == from && tr.To == to && tr.Duration == ticks {
			return true
		}
	}
	return false
}

func (t *Timeline) validateTransition(tr Transition, segs map[string]Segment) error {
	if tr.ID == "" {
		return invalid(clock.CatArgument, clock.Span{}, "transition with empty id")
	}
	from, okFrom := segs[tr.From]
	if !okFrom {
		return invalid(clock.CatGraph, clock.Span{}, "transition %s references missing segment %q", tr.ID, tr.From)
	}
	to, okTo := segs[tr.To]
	if !okTo {
		return invalid(clock.CatGraph, span(from), "transition %s references missing segment %q", tr.ID, tr.To)
	}
	if tr.From == tr.To {
		return invalid(clock.CatGraph, span(from), "transition %s links segment to itself", tr.ID)
	}
	if !tr.Kind.valid() {
		return invalid(clock.CatArgument, span(from), "transition %s has unknown kind %q", tr.ID, tr.Kind)
	}
	if tr.Duration < 0 {
		return invalid(clock.CatTimeline, span(from), "transition %s has negative duration %d", tr.ID, tr.Duration)
	}
	if tr.Kind != TransCut && tr.Duration == 0 {
		return invalid(clock.CatTimeline, span(from), "transition %s of kind %s needs positive duration", tr.ID, tr.Kind)
	}
	if limit := min(from.Duration, to.Duration); tr.Duration > limit {
		return invalid(clock.CatTimeline, span(from), "transition %s duration %d exceeds shorter segment %d",
			tr.ID, tr.Duration, limit)
	}
	return nil
}
