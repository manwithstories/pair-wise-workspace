// Package report aggregates synchronization, recovery and latency metrics
// and renders the terminal/JSON delivery report.
package report

import "sort"

// Metrics is the delivery summary emitted by the pipeline.
type Metrics struct {
	SyncErrorMS        float64  `json:"sync_error_ms"`
	LossRecoveryRate   float64  `json:"loss_recovery_rate"`
	IndexEntries       int      `json:"index_entries"`
	BoundaryMismatches int      `json:"boundary_mismatches"`
	P95LatencyMS       float64  `json:"p95_latency_ms"`
	RecoveredSegments  int      `json:"recovered_segments"`
	CorruptedRanges    int      `json:"corrupted_ranges"`
	UnrecoveredRanges  int      `json:"unrecovered_ranges"`
	IndexDigest        string   `json:"index_digest"`
	SyncTableBytes     int      `json:"sync_table_bytes"`
	AudioIssues        []string `json:"audio_issues,omitempty"`
	ColorConflicts     []string `json:"color_conflicts,omitempty"`
	Faults             []string `json:"faults,omitempty"`
}

// P95 returns the 95th percentile of vals (0 for empty input).
func P95(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	i := (95*len(s) + 99) / 100 // ceil(0.95*n)
	return s[i-1]
}

// TicksToMS converts 90 kHz clock ticks to milliseconds.
func TicksToMS(ticks int64) float64 {
	v := float64(ticks) / 90.0
	if v < 0 {
		return -v
	}
	return v
}
