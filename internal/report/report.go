// Package report renders terminal tables and JSON run reports.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"subtool/internal/model"
)

// CueRow is one per-cue output line, stable-sorted by cue ID.
type CueRow struct {
	CueID            string  `json:"cue_id"`
	StartFrame       int64   `json:"start_frame"`
	EndFrame         int64   `json:"end_frame"`
	StartMs          float64 `json:"start_ms"`
	EndMs            float64 `json:"end_ms"`
	RtDriftMs        float64 `json:"rt_drift_ms"`
	FrameSnapDriftMs float64 `json:"frame_snap_drift_ms"`
	Subframe         bool    `json:"subframe,omitempty"`
	Status           string  `json:"status"`
}

// Summary is the aggregate run report (also the --report JSON schema).
type Summary struct {
	TotalCues          int      `json:"total_cues"`
	EmittedCues        int      `json:"emitted_cues"`
	DroppedCues        int      `json:"dropped_cues"`
	AddedCues          int      `json:"added_cues"`
	MaxDriftMs         float64  `json:"max_drift_ms"`
	P99DriftMs         float64  `json:"p99_drift_ms"`
	FrameSnapDriftMs   float64  `json:"frame_snap_drift_ms"`
	TextMismatch       int      `json:"text_mismatch"`
	OverlapSplits      int      `json:"overlap_splits"`
	OverlapTrims       int      `json:"overlap_trims"`
	OverlapTrimmedMs   float64  `json:"overlap_trimmed_ms"`
	DetectedEncoding   string   `json:"detected_encoding"`
	EncodingConfidence float64  `json:"encoding_confidence"`
	QuarantinedFiles   int      `json:"quarantined_files"`
	QuarantinedCues    int      `json:"quarantined_cues"`
	Issues             []string `json:"issues,omitempty"`
}

// PrintRows writes one line per cue in deterministic ID order.
func PrintRows(w io.Writer, rows []CueRow) {
	sorted := append([]CueRow(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].CueID < sorted[j].CueID })
	for _, r := range sorted {
		fmt.Fprintf(w,
			"cue_id=%s start_frame=%d end_frame=%d start_ms=%.1f end_ms=%.1f rt_drift_ms=%.3f frame_snap_drift_ms=%.3f status=%s\n",
			r.CueID, r.StartFrame, r.EndFrame, r.StartMs, r.EndMs,
			r.RtDriftMs, r.FrameSnapDriftMs, r.Status)
	}
}

// PrintSummary writes the single-line aggregate to the terminal.
func PrintSummary(w io.Writer, s Summary) {
	fmt.Fprintf(w,
		"total_cues=%d emitted_cues=%d dropped_cues=%d added_cues=%d max_drift_ms=%.3f p99_drift_ms=%.3f frame_snap_drift_ms=%.3f text_mismatch=%d overlap_trimmed_ms=%.0f detected_encoding=%s quarantined_files=%d quarantined_cues=%d\n",
		s.TotalCues, s.EmittedCues, s.DroppedCues, s.AddedCues,
		s.MaxDriftMs, s.P99DriftMs, s.FrameSnapDriftMs, s.TextMismatch,
		s.OverlapTrimmedMs, s.DetectedEncoding, s.QuarantinedFiles, s.QuarantinedCues)
}

// WriteJSON emits the full report (summary + rows) as JSON.
func WriteJSON(w io.Writer, s Summary, rows []CueRow) error {
	doc := struct {
		Summary Summary  `json:"summary"`
		Rows    []CueRow `json:"rows"`
	}{s, rows}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// IssuesToStrings flattens model issues for the JSON report.
func IssuesToStrings(issues []model.Issue) []string {
	out := make([]string, len(issues))
	for i, is := range issues {
		out[i] = is.String()
	}
	return out
}
