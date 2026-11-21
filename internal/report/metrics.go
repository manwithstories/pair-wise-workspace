// Package report computes the acceptance metrics: end-to-end AV sync error,
// loss recovery rate and P95 latency. Every denominator is the actual injected
// loss count, so a clean stream reports recovery rate "n/a" instead of a
// misleading 1.0.
package report

import (
	"fmt"
	"sort"

	"mediasync/internal/model"
	"mediasync/internal/timeline"
	"mediasync/internal/transport"
)

// Metrics is the full acceptance snapshot for one pipeline run.
type Metrics struct {
	AVSyncMaxMs       float64
	LossInjected      int
	LossRecovered     int
	LossGaps          int
	LossRecoveryRate  string // formatted fraction or "n/a"
	P95LatencyMs      float64
	BoundaryMismatch  int
	FrameHashCount    int
	IncompleteTracks  []string
	PerRepRecovery    map[string]int
}

// LatencySample is one end-to-end latency observation in shared-clock ticks.
type LatencySample struct {
	Track    string
	Ticks    int64
	Clock    model.Clock
}

// Build collects metrics from the pipeline stages. lossInjected is the number
// of packets the synthetic network actually dropped.
func Build(tl *timeline.Timeline, repStats []transport.Stats,
	latencies []LatencySample, boundaryMismatches int, frameHashCount int,
	lossInjected int) Metrics {

	m := Metrics{
		AVSyncMaxMs:      tl.SyncMaxMs,
		BoundaryMismatch: boundaryMismatches,
		FrameHashCount:   frameHashCount,
		IncompleteTracks: tl.IncompleteTracks(),
		PerRepRecovery:   map[string]int{},
	}
	for _, s := range repStats {
		m.LossRecovered += s.Recovered
		m.LossGaps += s.Gaps
		m.PerRepRecovery[s.Rep] = s.Recovered
	}
	m.LossInjected = lossInjected
	switch {
	case lossInjected == 0:
		m.LossRecoveryRate = "n/a" // no losses: a numeric rate would be a false signal
	default:
		rate := float64(m.LossRecovered) / float64(lossInjected)
		m.LossRecoveryRate = fmt.Sprintf("%.3f", rate)
	}
	m.P95LatencyMs = percentileLatency(latencies, 95)
	return m
}

// percentileLatency computes the nearest-rank P95 in milliseconds. An empty
// sample set reports 0.
func percentileLatency(samples []LatencySample, pct float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	ms := make([]float64, 0, len(samples))
	for _, s := range samples {
		ms = append(ms, s.Clock.ToMillis(s.Ticks))
	}
	sort.Float64s(ms)
	rank := int(float64(len(ms))*pct/100.0 + 0.999999) // nearest-rank, 1-based
	if rank < 1 {
		rank = 1
	}
	if rank > len(ms) {
		rank = len(ms)
	}
	return ms[rank-1]
}

// Lines renders the metrics in the exact key=value format used by acceptance.
func (m Metrics) Lines() []string {
	return []string{
		fmt.Sprintf("AV_SYNC_MAX_MS=%.1fms", m.AVSyncMaxMs),
		fmt.Sprintf("LOSS_RECOVERY_RATE=%s", m.LossRecoveryRate),
		fmt.Sprintf("P95_LATENCY_MS=%.1f", m.P95LatencyMs),
		fmt.Sprintf("FRAME_HASH_COUNT=%d", m.FrameHashCount),
		fmt.Sprintf("BOUNDARY_MISMATCH_COUNT=%d", m.BoundaryMismatch),
	}
}
