package timeline

import (
	"sort"

	"mediamix/internal/core"
)

// CheckSegments verifies that per-track segment intervals do not overlap.
func CheckSegments(track string, spans []Span) []*core.Error {
	var errs []*core.Error
	for i := 1; i < len(spans); i++ {
		if !spans[i].Valid() {
			errs = append(errs, &core.Error{Code: core.CodeBadTimestamp, Track: track,
				Detail: "invalid segment interval", Fatal: true})
			continue
		}
		if spans[i].Overlaps(spans[i-1]) {
			errs = append(errs, &core.Error{Code: core.CodeOverlap, Track: track,
				Detail: "segments overlap", Fatal: true})
		}
	}
	return errs
}

// MergeSpans merges overlapping or touch-adjacent spans, sorted by start.
func MergeSpans(spans []Span) []Span {
	if len(spans) == 0 {
		return nil
	}
	cp := append([]Span(nil), spans...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Start < cp[j].Start })
	out := []Span{cp[0]}
	for _, s := range cp[1:] {
		last := &out[len(out)-1]
		if s.Start <= last.End {
			if s.End > last.End {
				last.End = s.End
			}
			continue
		}
		out = append(out, s)
	}
	return out
}
