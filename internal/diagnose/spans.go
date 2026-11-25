// Package diagnose merges fault spans reported by the analyzers into a
// minimal, sorted set of damaged ranges.
package diagnose

import "fieldqc/internal/fault"

// Merge combines overlapping or adjacent spans that share track and
// code, returning a sorted minimal set. Identical inputs always produce
// identical output.
func Merge(spans []fault.Span) []fault.Span {
	if len(spans) == 0 {
		return nil
	}
	sorted := append([]fault.Span(nil), spans...)
	fault.SortSpans(sorted)
	out := make([]fault.Span, 0, len(sorted))
	for _, s := range sorted {
		if n := len(out); n > 0 {
			last := &out[n-1]
			if last.Track == s.Track && last.Code == s.Code && s.Start.Cmp(last.End) <= 0 {
				if s.End.Cmp(last.End) > 0 {
					last.End = s.End
				}
				if last.Detail == "" {
					last.Detail = s.Detail
				}
				continue
			}
		}
		out = append(out, s)
	}
	return out
}
