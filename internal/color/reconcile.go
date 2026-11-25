// Package color reconciles color metadata across tracks. Conflicts
// (e.g. BT.709 vs BT.2020) keep every track's source and produce
// diagnostics — values are never silently overwritten.
package color

import (
	"fmt"
	"sort"
	"strings"

	"fieldqc/internal/clock"
	"fieldqc/internal/fault"
)

// Meta is one track's color metadata; Source identifies the track.
type Meta struct {
	Source    string
	Primaries string // e.g. "bt709", "bt2020"
	Transfer  string
	Matrix    string
	FullRange bool
}

// Conflict records one metadata field whose values disagree.
type Conflict struct {
	Field  string
	Values map[string]string // track -> value
}

// Reconcile returns the unanimous metadata, or the zero Meta plus all
// conflicts when tracks disagree. Per-track sources are preserved in
// the conflicts; nothing is silently overridden.
func Reconcile(metas []Meta) (Meta, []Conflict) {
	if len(metas) == 0 {
		return Meta{}, nil
	}
	fields := []struct {
		name string
		get  func(Meta) string
	}{
		{"primaries", func(m Meta) string { return m.Primaries }},
		{"transfer", func(m Meta) string { return m.Transfer }},
		{"matrix", func(m Meta) string { return m.Matrix }},
		{"range", func(m Meta) string {
			if m.FullRange {
				return "full"
			}
			return "limited"
		}},
	}
	var conflicts []Conflict
	for _, f := range fields {
		vals := make(map[string]string, len(metas))
		same := true
		for _, m := range metas {
			v := f.get(m)
			vals[m.Source] = v
			if v != f.get(metas[0]) {
				same = false
			}
		}
		if !same {
			conflicts = append(conflicts, Conflict{Field: f.name, Values: vals})
		}
	}
	if len(conflicts) > 0 {
		return Meta{}, conflicts
	}
	return metas[0], nil
}

// Spans converts conflicts to fault spans on the shared clock over
// [start, end); details are deterministic (sorted track names).
func Spans(conflicts []Conflict, start, end clock.Ratio) []fault.Span {
	var out []fault.Span
	for _, c := range conflicts {
		tracks := make([]string, 0, len(c.Values))
		for tr := range c.Values {
			tracks = append(tracks, tr)
		}
		sort.Strings(tracks)
		var b strings.Builder
		fmt.Fprintf(&b, "%s conflict:", c.Field)
		for _, tr := range tracks {
			fmt.Fprintf(&b, " %s=%s", tr, c.Values[tr])
		}
		out = append(out, fault.Span{
			Track: "color", Code: fault.CodeColorConflict,
			Start: start, End: end, Detail: b.String(),
		})
	}
	return out
}
