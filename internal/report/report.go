package report

import (
	"fmt"
	"io"
)

// Failure is one classified verification failure.
type Failure struct {
	Category  string `json:"category"`
	Rendition string `json:"rendition"`
	Sequence  int64  `json:"sequence"`
	Detail    string `json:"detail,omitempty"`
}

// Rendition is one row of the verification report.
type Rendition struct {
	Rendition   string `json:"rendition"`
	Segments    int    `json:"segments"`
	BitrateKbps int    `json:"bitrate_kbps"`
	DurationMs  int64  `json:"duration_ms"`
	DeviationMs int64  `json:"deviation_ms"`
	Failures    int    `json:"failures"`
}

// Summary is the aggregate trailer of the report.
type Summary struct {
	Renditions    int `json:"renditions"`
	SegmentsTotal int `json:"segments_total"`
	Gaps          int `json:"gaps"`
	Failures      int `json:"failures"`
}

// Report is the full verification report (JSON + terminal table).
type Report struct {
	Renditions []Rendition `json:"renditions"`
	Summary    Summary     `json:"summary"`
	Failures   []Failure   `json:"failures"`
}

// Finalize recomputes per-rendition failure counts and the summary.
func (r *Report) Finalize() {
	counts := map[string]int{}
	for _, f := range r.Failures {
		counts[f.Rendition]++
	}
	r.Summary.Renditions = len(r.Renditions)
	r.Summary.Failures = len(r.Failures)
	r.Summary.SegmentsTotal = 0
	for i := range r.Renditions {
		rr := &r.Renditions[i]
		rr.Failures = counts[rr.Rendition]
		r.Summary.SegmentsTotal += rr.Segments
	}
}

// AddGaps accumulates the gap total used for the degraded-release exit code.
func (r *Report) AddGaps(n int) { r.Summary.Gaps += n }

// WriteTable renders the fixed-width terminal table.
func (r *Report) WriteTable(w io.Writer) {
	for _, rr := range r.Renditions {
		fmt.Fprintf(w, "rendition=%-6s segments=%-4d bitrate_kbps=%-6d duration_ms=%-8d deviation_ms=%-6d failures=%d\n",
			rr.Rendition, rr.Segments, rr.BitrateKbps, rr.DurationMs, rr.DeviationMs, rr.Failures)
	}
	fmt.Fprintf(w, "summary renditions=%d segments_total=%d gaps=%d failures=%d\n",
		r.Summary.Renditions, r.Summary.SegmentsTotal, r.Summary.Gaps, r.Summary.Failures)
}

// ExitCode maps the report to the release-gate exit code:
// 0 all pass, 2 gaps only (degraded release allowed), 3 blocking failures.
func (r *Report) ExitCode() int {
	if r.Summary.Failures > 0 {
		return 3
	}
	if r.Summary.Gaps > 0 {
		return 2
	}
	return 0
}
