package metrics

import (
	"strings"
	"testing"

	"avcheck/internal/flow"
	"avcheck/internal/recovery"
)

func TestP95NearestRank(t *testing.T) {
	var l Latency
	for i := 1; i <= 20; i++ {
		l.Add(float64(i)) // 1..20 ms
	}
	// ceil(0.95*20) = 19th ranked value
	if got := l.P95(); got != 19 {
		t.Fatalf("p95=%v, want 19", got)
	}
	var empty Latency
	if empty.P95() != 0 {
		t.Fatal("empty latency p95 must be 0")
	}
}

func TestBuildRecoveryRate(t *testing.T) {
	rec := recovery.Stats{Interpolated: 9, SilenceFilled: 1}
	rep := Build(0, rec, flow.Snapshot{}, &Latency{}, 0, 0, nil, 0)
	if rep.LossRecoveryRate != 0.9 {
		t.Fatalf("rate=%v, want 0.9", rep.LossRecoveryRate)
	}

	// No losses at all -> perfect 1.0, not a divide-by-zero.
	rep2 := Build(0, recovery.Stats{}, flow.Snapshot{}, &Latency{}, 0, 0, nil, 0)
	if rep2.LossRecoveryRate != 1.0 {
		t.Fatalf("rate=%v, want 1.0", rep2.LossRecoveryRate)
	}
}

func TestPassGates(t *testing.T) {
	th := DefaultThresholds(262144)
	good := Report{
		SyncErrorSamples: 1, LossRecoveryRate: 0.95, P95LatencyMS: 120,
		PeakQueueBytes: 200000, LostBlocks: 0,
	}
	if !good.Pass(th) {
		t.Fatal("healthy report must pass")
	}
	bad := good
	bad.P95LatencyMS = 250
	if bad.Pass(th) {
		t.Fatal("p95 above ceiling must fail")
	}
	bad = good
	bad.PeakQueueBytes = 262145
	if bad.Pass(th) {
		t.Fatal("queue over ceiling must fail")
	}
	bad = good
	bad.LostBlocks = 1
	if bad.Pass(th) {
		t.Fatal("any lost block must fail")
	}
	bad = good
	bad.LossRecoveryRate = 0.8
	if bad.Pass(th) {
		t.Fatal("recovery below 90% must fail")
	}
}

func TestReportLineShape(t *testing.T) {
	var lat Latency
	lat.Add(10)
	lat.Add(40)
	rep := Build(1, recovery.Stats{Interpolated: 2},
		flow.Snapshot{PeakBytes: 1234, Enqueued: 8, BackpressureEv: 2},
		&lat, 300, 0xDEADBEEF, map[uint32]int{1: 48000}, 0)
	line := rep.Line()
	for _, want := range []string{
		"sync_error_samples=1",
		"loss_recovery_rate=1.000",
		"p95_latency_ms=40.000",
		"peak_queue_bytes=1234",
		"frame_count=300",
		"frame_hash=0x00000000deadbeef",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("line %q missing %q", line, want)
		}
	}
}
