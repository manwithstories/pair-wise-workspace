package core

import (
	"sort"
	"strings"
)

// Range is a half-open [Start, End) interval on the unified timeline that
// records a fault: a timeline conflict (Source "timeline") or damaged
// segment data (Source "segment"). Ranges are the shared error currency:
// timeline errors flow into the index, segment errors flow into keyframe
// results and the provenance record.
type Range struct {
	Start  Rat    `json:"start"`
	End    Rat    `json:"end"`
	Source string `json:"source"`
	Reason string `json:"reason"`
}

// Contains reports whether t falls inside the half-open range.
func (r Range) Contains(t Rat) bool {
	return r.Start.Cmp(t) <= 0 && t.Cmp(r.End) < 0
}

// MergeRanges sorts ranges by (Source, Start, End, Reason) and merges
// overlapping or touching ranges that share the same Source, joining
// distinct reasons with ";". The output is deterministic.
func MergeRanges(rs []Range) []Range {
	if len(rs) == 0 {
		return nil
	}
	sorted := append([]Range(nil), rs...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if c := a.Start.Cmp(b.Start); c != 0 {
			return c < 0
		}
		if c := a.End.Cmp(b.End); c != 0 {
			return c < 0
		}
		return a.Reason < b.Reason
	})
	out := []Range{sorted[0]}
	for _, r := range sorted[1:] {
		last := &out[len(out)-1]
		if r.Source == last.Source && r.Start.Cmp(last.End) <= 0 {
			if r.End.Cmp(last.End) > 0 {
				last.End = r.End
			}
			last.Reason = joinReasons(last.Reason, r.Reason)
			continue
		}
		out = append(out, r)
	}
	return out
}

func joinReasons(a, b string) string {
	if a == b || b == "" {
		return a
	}
	if a == "" {
		return b
	}
	for part := range strings.SplitSeq(a, ";") {
		if part == b {
			return a
		}
	}
	return a + ";" + b
}
