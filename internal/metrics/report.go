// Package metrics collects the cross-subsystem counters surfaced by the
// pipeline and evaluates them against the acceptance thresholds.
package metrics

import (
	"sort"
)

// Thresholds encodes the acceptance gates used to choose the exit code.
type Thresholds struct {
	MinRecoveredPct float64 // minimum recovery percentage on damaged runs
	MaxSyncMillis   float64 // maximum audio/video sync error
	MaxLatencyP95   float64 // maximum 95th-percentile latency in ms
}

// DefaultThresholds matches the project acceptance criteria.
func DefaultThresholds() Thresholds {
	return Thresholds{MinRecoveredPct: 80.0, MaxSyncMillis: 20.0, MaxLatencyP95: 40.0}
}

// Report is the single metrics object the pipeline emits.
type Report struct {
	Frames        int     `json:"frames"`
	RecoveredPct  float64 `json:"recovered_pct"`
	SyncMaxMillis float64 `json:"sync_max_ms"`
	LatencyP95Ms  float64 `json:"latency_p95_ms"`
	BadRecords    int     `json:"bad_records"`
	IndexFallback bool    `json:"index_fallback"`
	OutputSHA256  string  `json:"output_sha256"`

	// Supporting counters kept for terminal diagnostics (not in the required
	// output contract but useful for operators).
	LostPackets     int `json:"-"`
	LostMedia       int `json:"-"`
	RecoveredFEC    int `json:"-"`
	Unrecoverable   int `json:"-"`
	BufferOverflows int `json:"-"`
	LateDrops       int `json:"-"`
	AudioBlocks     int `json:"-"`
	AudioGaps       int `json:"-"`
	ParserErrors    int `json:"-"`
}

// Percentile computes the p-th percentile (0..100) of a sorted copy of the
// input using nearest-rank. An empty input returns 0.
func Percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	rank := (p / 100.0) * float64(len(sorted))
	idx := int(rank)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// Evaluate compares the report against thresholds. The returned verdict drives
// the process exit code:
//
//	verdict 0 -> metrics pass
//	verdict 2 -> input unrecoverable (no usable frames at all)
//	verdict 3 -> metrics below target
func (r *Report) Evaluate(t Thresholds, damaged bool) (verdict int, reasons []string) {
	if r.Frames == 0 {
		return 2, []string{"no usable frames could be parsed"}
	}
	if damaged {
		if r.RecoveredPct < t.MinRecoveredPct {
			reasons = append(reasons, "recovered_pct below minimum")
		}
	}
	if r.SyncMaxMillis > t.MaxSyncMillis {
		reasons = append(reasons, "sync_max_ms above limit")
	}
	if r.LatencyP95Ms > t.MaxLatencyP95 {
		reasons = append(reasons, "latency_p95_ms above limit")
	}
	if len(reasons) > 0 {
		return 3, reasons
	}
	return 0, nil
}
