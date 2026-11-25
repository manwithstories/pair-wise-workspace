// Package metrics aggregates the counters every pipeline stage contributes and
// renders the single terminal report operators use to judge recoverability.
package metrics

import (
	"encoding/json"
	"io"
)

// Report is the exact field set the demo contract requires. JSON tags match
// the documented output names one-for-one.
type Report struct {
	TargetBPS        int64   `json:"target_bps"`
	ActualBPS        int64   `json:"actual_bps"`
	DeviationPct     float64 `json:"deviation_pct"`
	Segments         int     `json:"segments"`
	CCErrors         int     `json:"cc_errors"`
	PSICRCRejects    int     `json:"psi_crc_rejects"`
	SilencePrecision float64 `json:"silence_precision"`
	SilenceRecall    float64 `json:"silence_recall"`
	SilenceF1        float64 `json:"silence_f1"`
	DuplicateHits    int     `json:"duplicate_hits"`
	ManifestSkewMS   float64 `json:"manifest_skew_ms"`
	BufferPeakBytes  int64   `json:"buffer_peak_bytes"`
	ResumeHash       string  `json:"resume_hash"`
	// Recoverable distinguishes exit code 0 (clean/recovered) from 2
	// (unrecoverable input). Unsupported codecs and truncation are recorded
	// here rather than misclassified.
	UnsupportedCodecs int      `json:"unsupported_codecs"`
	Truncated         bool     `json:"truncated"`
	SilenceIntervals  int      `json:"silence_intervals"`
	Notes             []string `json:"notes,omitempty"`
}

// New returns a zeroed report seeded with the target bitrate.
func New(targetBPS int64) *Report {
	return &Report{TargetBPS: targetBPS}
}

// SetActual derives actual_bps from bytes consumed over the media duration
// and computes deviation_pct against the target.
func (r *Report) SetActual(totalBytes int64, durationTicks int64, ticksPerSecond int64) {
	if durationTicks <= 0 {
		return
	}
	seconds := float64(durationTicks) / float64(ticksPerSecond)
	r.ActualBPS = int64(float64(totalBytes*8) / seconds)
	if r.TargetBPS > 0 {
		r.DeviationPct = (float64(r.ActualBPS-r.TargetBPS) / float64(r.TargetBPS)) * 100
	}
}

// Note appends a human-readable diagnostic line.
func (r *Report) Note(s string) { r.Notes = append(r.Notes, s) }

// WriteJSON emits the report with stable field ordering.
func WriteJSON(w io.Writer, r *Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
