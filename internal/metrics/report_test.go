package metrics

import (
	"testing"
	"time"

	"mediamix/internal/clock"
)

func TestPercentile(t *testing.T) {
	vals := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := Percentile(vals, 95); got != 10 {
		t.Fatalf("p95=%v want 10", got)
	}
	if got := Percentile(vals, 50); got != 5 {
		t.Fatalf("p50=%v want 5", got)
	}
	if got := Percentile(nil, 95); got != 0 {
		t.Fatalf("empty p95=%v want 0", got)
	}
}

func TestRecoveryRate(t *testing.T) {
	if got := RecoveryRate(0, 0); got != 100 {
		t.Fatalf("no loss should be 100%%, got %v", got)
	}
	if got := RecoveryRate(20, 19); got != 95 {
		t.Fatalf("19/20 = %v want 95", got)
	}
	if got := RecoveryRate(20, 25); got != 100 {
		t.Fatalf("over-count must clamp to 100%%, got %v", got)
	}
}

func TestAVSync(t *testing.T) {
	samples := []Sample{
		{MediaTick: clock.FromTime(100 * time.Millisecond), VideoTick: clock.FromTime(102 * time.Millisecond)},
		{MediaTick: clock.FromTime(200 * time.Millisecond), VideoTick: clock.FromTime(197 * time.Millisecond)},
	}
	if got := AVSync(samples); got != 3.0 {
		t.Fatalf("av sync worst=%v want 3ms", got)
	}
}

func TestBuildAndEvaluate(t *testing.T) {
	samples := []Sample{
		{ArrivalMS: 0, EmittedMS: 10, MediaTick: clock.FromTime(0), VideoTick: clock.FromTime(0)},
		{ArrivalMS: 5, EmittedMS: 20, MediaTick: clock.FromTime(20 * time.Millisecond), VideoTick: clock.FromTime(21 * time.Millisecond)},
	}
	r := Build("normal", samples, 20, 20, 4096, "abc")
	if r.AVSyncMS != 1 || r.LossRecoveryPct != 100 || r.QueueBytesPeak != 4096 {
		t.Fatalf("unexpected report: %+v", r)
	}
	if v := Evaluate(r); !v.OK {
		t.Fatalf("healthy report should pass: %v", v.Reasons)
	}

	bad := r
	bad.AVSyncMS = 6
	bad.LossRecoveryPct = 90
	bad.P95LatencyMS = 300
	bad.QueueBytesPeak = MaxQueuePeakBytes + 1
	v := Evaluate(bad)
	if v.OK || len(v.Reasons) != 4 {
		t.Fatalf("expected 4 violations, got %v", v.Reasons)
	}
}
