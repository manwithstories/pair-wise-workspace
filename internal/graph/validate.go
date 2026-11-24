package graph

import (
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
