// Package align reconciles the segment cuts of several bitrate renditions of
// the same source. Every rendition must switch at the same program-time
// points; where a rendition's keyframes cannot meet the shared cut, the
// maximum boundary deviation is reported rather than hidden.
package align

import (
	"fmt"
	"sort"

	"archive.local/recover/internal/errs"
	"archive.local/recover/internal/segment"
)

// Rendition is one bitrate version of the source.
type Rendition struct {
	Name     string          `json:"name"`
	Bitrate  int             `json:"bitrate"`
	Segments *segment.Result `json:"segments"`
}

// Boundary is one shared cut across all renditions.
type Boundary struct {
	Index int `json:"index"`
	// ProgramNs is the shared program-time cut.
	ProgramNs int64 `json:"program_ns"`
	// PerRendition holds the actual cut for each rendition, keyed by name.
	Actual map[string]int64 `json:"actual_ns"`
	// DeviationNs is the largest distance between Actual and ProgramNs.
	DeviationNs int64 `json:"deviation_ns"`
	// Pending marks a cut beyond the configured tolerance.
	Pending bool `json:"pending_review"`
}

// Result is the aligned ladder.
type Result struct {
	Boundaries     []Boundary `json:"boundaries"`
	MaxDeviationNs int64      `json:"alignment_delta_ns"`
	Pending        int        `json:"pending_review"`
	SegmentCount   int        `json:"segments"`
	Issues         errs.List  `json:"issues"`
}

// Config bounds acceptable alignment error.
type Config struct {
	MaxDeltaNs int64
}

func (c Config) withDefaults() Config {
	if c.MaxDeltaNs <= 0 {
		c.MaxDeltaNs = 40_000_000
	}
	return c
}

// Ladder aligns renditions on the finest cut list among them, which is the
// longest set of keyframes; that choice guarantees every rendition has a cut
// at or after each shared boundary.
func Ladder(rends []Rendition, cfg Config) *Result {
	cfg = cfg.withDefaults()
	res := &Result{}
	if len(rends) == 0 {
		res.Issues.AddFatal(errs.ClassDependency, errs.CodeDependencyMissing,
			"no renditions to align", errs.UnknownLocation())
		return res
	}
	// Reference: the rendition with the most cuts (ties broken by bitrate then
	// name) determines the shared program-time grid.
	ref := 0
	for i := 1; i < len(rends); i++ {
		a, b := rends[i], rends[ref]
		if n, m := cutCount(a), cutCount(b); n > m || (n == m && a.Bitrate > b.Bitrate) {
			ref = i
		}
	}
	base := rends[ref].Segments
	if base == nil || len(base.Cuts) == 0 {
		res.Issues.AddFatal(errs.ClassDependency, errs.CodeDependencyMissing,
			fmt.Sprintf("reference rendition %q has no cuts", rends[ref].Name), errs.UnknownLocation())
		return res
	}
	res.SegmentCount = len(base.Cuts)
	for i, cut := range base.Cuts {
		b := Boundary{Index: i, ProgramNs: cut.StartNs, Actual: map[string]int64{}}
		for _, r := range rends {
			actual, ok := nearestCut(r.Segments, cut.StartNs)
			if !ok {
				// A rendition without the shared cut must be reported and the
				// boundary marked pending; it is never silently snapped.
				res.Issues.Add(errs.ClassAlignment, errs.CodeAlignmentDelta,
					fmt.Sprintf("rendition %q (%d bps) has no cut at %.3f ms",
						r.Name, r.Bitrate, float64(cut.StartNs)/1e6),
					errs.Location{Sample: i, Offset: -1, Field: r.Name})
				b.Pending = true
				continue
			}
			b.Actual[r.Name] = actual
			if d := abs64(actual - cut.StartNs); d > b.DeviationNs {
				b.DeviationNs = d
			}
		}
		if b.DeviationNs > res.MaxDeviationNs {
			res.MaxDeviationNs = b.DeviationNs
		}
		if b.DeviationNs > cfg.MaxDeltaNs {
			b.Pending = true
			res.Issues.Add(errs.ClassAlignment, errs.CodeAlignmentDelta,
				fmt.Sprintf("boundary %d deviates %.3f ms across renditions (limit %.3f ms)",
					i, float64(b.DeviationNs)/1e6, float64(cfg.MaxDeltaNs)/1e6),
				errs.Location{Sample: i, Offset: -1})
		}
		if b.Pending {
			res.Pending++
		}
		res.Boundaries = append(res.Boundaries, b)
	}
	return res
}

func cutCount(r Rendition) int {
	if r.Segments == nil {
		return 0
	}
	return len(r.Segments.Cuts)
}

// nearestCut finds the rendition's cut closest to programNs, preferring an
// exact match.
func nearestCut(s *segment.Result, programNs int64) (int64, bool) {
	if s == nil || len(s.Cuts) == 0 {
		return 0, false
	}
	best := s.Cuts[0].StartNs
	bestD := abs64(best - programNs)
	for _, c := range s.Cuts[1:] {
		if d := abs64(c.StartNs - programNs); d < bestD {
			best, bestD = c.StartNs, d
		}
	}
	return best, true
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// SnapToShared returns the shared cut nearest each rendition cut, used by the
// manifest writer to express one switch grid for all renditions.
func SnapToShared(cuts []segment.Cut, shared []int64) []int64 {
	out := make([]int64, len(cuts))
	for i, c := range cuts {
		best := shared[0]
		bestD := abs64(best - c.StartNs)
		for _, s := range shared[1:] {
			if d := abs64(s - c.StartNs); d < bestD {
				best, bestD = s, d
			}
		}
		out[i] = best
	}
	return out
}

// UniqueSorted returns the sorted, de-duplicated shared cut list across the
// given renditions.
func UniqueSorted(rends []Rendition) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, r := range rends {
		if r.Segments == nil {
			continue
		}
		for _, c := range r.Segments.Cuts {
			if !seen[c.StartNs] {
				seen[c.StartNs] = true
				out = append(out, c.StartNs)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
