package align

import (
	"math"

	"dashpack/internal/mp4"
)

// Report is the verify/pack summary emitted as JSON.
type Report struct {
	Representations  int      `json:"representations"`
	SegmentsTotal    int      `json:"segments_total"`
	TemplateExpanded int      `json:"template_expanded"`
	TemplateFailed   int      `json:"template_failed"`
	MaxDeviationMs   float64  `json:"max_deviation_ms"`
	MeanDeviationMs  float64  `json:"mean_deviation_ms"`
	Gaps             int      `json:"gaps"`
	Overlaps         int      `json:"overlaps"`
	ShortTail        int      `json:"short_tail"`
	NonMonotonic     int      `json:"non_monotonic"`
	Reference        string   `json:"reference"`
	OutOfTolerance   []string `json:"out_of_tolerance,omitempty"`
}

// BuildReport aggregates scan timelines, the alignment result and template
// expansion counters into the final report.
func BuildReport(metas []RepMeta, tls map[string]*mp4.Timeline, res *Result, expanded, failed int) *Report {
	rep := &Report{
		Representations:  len(metas),
		TemplateExpanded: expanded,
		TemplateFailed:   failed,
		Reference:        res.ReferenceID,
		OutOfTolerance:   res.OutOfTolerance,
		MaxDeviationMs:   res.MaxDeviationMs,
		MeanDeviationMs:  res.MeanDeviationMs,
	}
	for _, m := range metas {
		tl := tls[m.ID]
		if tl == nil {
			continue
		}
		rep.SegmentsTotal += len(tl.Segments)
		rep.Gaps += tl.Gaps
		rep.Overlaps += tl.Overlaps
		rep.NonMonotonic += tl.NonMonotonic
		if tl.ShortTail {
			rep.ShortTail++
		}
	}
	return rep
}

// Round3 rounds to milliseconds with three decimals for stable output.
func Round3(f float64) float64 {
	return math.Round(f*1000) / 1000
}
