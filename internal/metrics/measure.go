// Package metrics computes sync, recovery, latency, and index quality metrics.
package metrics

import (
	"fmt"
	"sort"

	"mediad/internal/recover"
)

// Acceptance thresholds from the demo specification.
const (
	MaxSyncMS          = 20.0
	MinRecoveryPct     = 99.0
	MaxLatencyP95MS    = 180.0
	RequiredSeekMisses = 0
	RequiredMismatch   = 0
)

// Metrics is the stable terminal/JSON summary.
type Metrics struct {
	SyncMaxMS        float64 `json:"sync_max_ms"`
	LossRecoveryPct  float64 `json:"loss_recovery_pct"`
	LatencyP95MS     float64 `json:"latency_p95_ms"`
	IndexSeekMiss    int     `json:"index_seek_miss"`
	BoundaryMismatch int     `json:"boundary_mismatch"`
	OutputSHA256     string  `json:"output_sha256"`

	Tracks             int `json:"tracks,omitempty"`
	Packets            int `json:"packets,omitempty"`
	LostPackets        int `json:"lost_packets,omitempty"`
	RecoveredPackets   int `json:"recovered_packets,omitempty"`
	UnrecoveredPackets int `json:"unrecovered_packets,omitempty"`
	Retransmissions    int `json:"retransmissions,omitempty"`
}

// Compute aggregates per-track recovery reports and cross-check results.
func Compute(syncMaxNS int64, reports []*recover.Report, seekMisses, boundaryMismatches int, sha256 string) Metrics {
	m := Metrics{
		SyncMaxMS:        float64(syncMaxNS) / 1e6,
		IndexSeekMiss:    seekMisses,
		BoundaryMismatch: boundaryMismatches,
		OutputSHA256:     sha256,
		Tracks:           len(reports),
	}
	var latencies []int
	for _, r := range reports {
		if r == nil {
			continue
		}
		m.Packets += r.Packets
		m.LostPackets += r.LostPackets
		m.RecoveredPackets += r.RecoveredPackets
		m.UnrecoveredPackets += r.UnrecoveredPackets
		m.Retransmissions += r.Retransmissions
		for _, s := range r.Stats {
			latencies = append(latencies, s.LatencyMS)
		}
	}
	if m.LostPackets > 0 {
		m.LossRecoveryPct = 100 * float64(m.RecoveredPackets) / float64(m.LostPackets)
	} else {
		m.LossRecoveryPct = 100
	}
	m.LatencyP95MS = percentile95(latencies)
	return m
}

// Failures returns threshold violations. An empty slice means acceptance passed.
func (m Metrics) Failures() []string {
	var out []string
	if m.SyncMaxMS > MaxSyncMS {
		out = append(out, fmt.Sprintf("sync_max_ms=%.3f > %.0f", m.SyncMaxMS, MaxSyncMS))
	}
	if m.LossRecoveryPct < MinRecoveryPct {
		out = append(out, fmt.Sprintf("loss_recovery_pct=%.3f < %.0f", m.LossRecoveryPct, MinRecoveryPct))
	}
	if m.LatencyP95MS > MaxLatencyP95MS {
		out = append(out, fmt.Sprintf("latency_p95_ms=%.3f > %.0f", m.LatencyP95MS, MaxLatencyP95MS))
	}
	if m.IndexSeekMiss != RequiredSeekMisses {
		out = append(out, fmt.Sprintf("index_seek_miss=%d != 0", m.IndexSeekMiss))
	}
	if m.BoundaryMismatch != RequiredMismatch {
		out = append(out, fmt.Sprintf("boundary_mismatch=%d != 0", m.BoundaryMismatch))
	}
	return out
}

// Pass reports whether all acceptance thresholds are met.
func (m Metrics) Pass() bool { return len(m.Failures()) == 0 }

// KeyValueLines emits the exact greppable metric keys required by acceptance.
func (m Metrics) KeyValueLines() []string {
	return []string{
		fmt.Sprintf("sync_max_ms=%.3f", m.SyncMaxMS),
		fmt.Sprintf("loss_recovery_pct=%.3f", m.LossRecoveryPct),
		fmt.Sprintf("latency_p95_ms=%.3f", m.LatencyP95MS),
		fmt.Sprintf("index_seek_miss=%d", m.IndexSeekMiss),
		fmt.Sprintf("boundary_mismatch=%d", m.BoundaryMismatch),
		fmt.Sprintf("output_sha256=%s", m.OutputSHA256),
	}
}

func percentile95(values []int) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int(nil), values...)
	sort.Ints(sorted)
	// Nearest-rank method, ceil(0.95*n), one-based.
	rank := (95*len(sorted) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return float64(sorted[rank-1])
}
