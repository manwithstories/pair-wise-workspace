package report

import (
	"strings"
	"testing"

	"mediasync/internal/metadata"
	"mediasync/internal/model"
	"mediasync/internal/timeline"
	"mediasync/internal/transport"
)

func emptyTimeline() *timeline.Timeline {
	return &timeline.Timeline{Clock: model.Clock90k, Tracks: map[string]metadata.ResolvedTrack{}}
}

// TestRecoveryRateNAOnZeroLoss enforces the no-false-signal rule.
func TestRecoveryRateNAOnZeroLoss(t *testing.T) {
	tl := emptyTimeline()
	m := Build(tl, nil, nil, 0, 10, 0)
	if m.LossRecoveryRate != "n/a" {
		t.Fatalf("zero injected loss must report n/a, got %q", m.LossRecoveryRate)
	}
}

func TestRecoveryRateAndP95(t *testing.T) {
	tl := emptyTimeline()
	tl.SyncMaxMs = 23.5
	stats := []transport.Stats{
		{Rep: "high", Recovered: 8, Gaps: 2},
		{Rep: "low", Recovered: 1, Gaps: 1},
	}
	// Ten latency samples in milliseconds-equivalent ticks at 1000Hz.
	var samples []LatencySample
	for i := 1; i <= 10; i++ {
		samples = append(samples, LatencySample{Track: "v", Ticks: int64(i * 10), Clock: model.Clock{TickRate: 1000}})
	}
	m := Build(tl, stats, samples, 1, 42, 12)
	if m.LossRecoveryRate != "0.750" {
		t.Fatalf("want 0.750 (9 recovered / 12 injected), got %q", m.LossRecoveryRate)
	}
	if m.LossGaps != 3 || m.LossRecovered != 9 {
		t.Fatalf("counters wrong: %+v", m)
	}
	if m.P95LatencyMs != 100.0 { // nearest-rank P95 of 10..100 is the 10th value
		t.Fatalf("P95 should be 100ms, got %.1f", m.P95LatencyMs)
	}
	if m.AVSyncMaxMs != 23.5 || m.FrameHashCount != 42 || m.BoundaryMismatch != 1 {
		t.Fatalf("pass-through metrics wrong: %+v", m)
	}
	if m.PerRepRecovery["high"] != 8 {
		t.Fatalf("per-rep recovery missing: %+v", m.PerRepRecovery)
	}
	lines := strings.Join(m.Lines(), "\n")
	for _, key := range []string{"AV_SYNC_MAX_MS=", "LOSS_RECOVERY_RATE=", "P95_LATENCY_MS=",
		"FRAME_HASH_COUNT=", "BOUNDARY_MISMATCH_COUNT="} {
		if !strings.Contains(lines, key) {
			t.Fatalf("acceptance line missing %q in:\n%s", key, lines)
		}
	}
}

func TestP95EmptyAndIncompleteTracks(t *testing.T) {
	tl := emptyTimeline()
	tl.Incomplete = []string{"b", "a"}
	m := Build(tl, nil, nil, 0, 0, 0)
	if m.P95LatencyMs != 0 || m.LossRecoveryRate != "n/a" {
		t.Fatalf("empty inputs must yield zero latency and n/a rate: %+v", m)
	}
	if got := strings.Join(m.IncompleteTracks, ","); got != "a,b" {
		t.Fatalf("incomplete tracks must be sorted: %s", got)
	}
}
