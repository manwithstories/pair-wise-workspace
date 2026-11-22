package metrics

import (
	"testing"

	"mediad/internal/model"
)

func TestPercentileNearestRank(t *testing.T) {
	vals := []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	got := percentile(vals, 95)
	// 最近秩：ceil(0.95*10)=10 -> 100。
	if got != 100 {
		t.Fatalf("p95=%v, want 100", got)
	}
	if got := percentile(vals, 50); got != 50 {
		t.Fatalf("p50=%v, want 50", got)
	}
	if got := percentile(nil, 95); got != 0 {
		t.Fatalf("empty p95=%v, want 0", got)
	}
}

func TestAggregatorSnapshot(t *testing.T) {
	a := NewAggregator()
	for i := 0; i < 20; i++ {
		a.ObserveLatency(float64(10 + i)) // 10..29
		a.ObserveSyncError(float64(i % 20))
	}
	corrupt := []model.CorruptRange{{StartSample: 0, EndSample: 480, StartMs: 0, EndMs: 10}}
	rep := a.Snapshot(0.8, 160, 8, 25, corrupt)
	if rep.RecoveryRate != 0.8 {
		t.Fatalf("recovery=%v", rep.RecoveryRate)
	}
	if rep.KeyframeGapMS != 160 || rep.QueuePeak != 8 || rep.BackpressureMS != 25 {
		t.Fatalf("scalar fields wrong: %+v", rep)
	}
	if rep.P95LatencyMS != 28 {
		t.Fatalf("p95=%v, want 28 (nearest-rank ceil(0.95*20)=19th value)", rep.P95LatencyMS)
	}
	if len(rep.CorruptRanges) != 1 {
		t.Fatalf("corrupt ranges not passed through: %+v", rep.CorruptRanges)
	}
	// 同步误差均值 = 0..19 的平均。
	if rep.SyncErrorMS != 9.5 {
		t.Fatalf("sync mean=%v, want 9.5", rep.SyncErrorMS)
	}
}

func TestNilCorruptRangesBecomesEmpty(t *testing.T) {
	a := NewAggregator()
	rep := a.Snapshot(1, 0, 0, 0, nil)
	if rep.CorruptRanges == nil || len(rep.CorruptRanges) != 0 {
		t.Fatalf("corrupt ranges must be empty non-nil, got %#v", rep.CorruptRanges)
	}
}

func TestObserveLatencyClampsNegative(t *testing.T) {
	a := NewAggregator()
	a.ObserveLatency(-5)
	a.ObserveSyncError(-12)
	if got := a.MaxSyncError(); got != 12 {
		t.Fatalf("abs sync error=%v, want 12", got)
	}
	rep := a.Snapshot(1, 0, 0, 0, nil)
	if rep.P95LatencyMS != 0 {
		t.Fatalf("negative latency must clamp to 0, got %v", rep.P95LatencyMS)
	}
}
