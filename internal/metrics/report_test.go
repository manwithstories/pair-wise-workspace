package metrics

import "testing"

func TestPercentileNearestRank(t *testing.T) {
	values := []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	if got := Percentile(values, 95); got < 90 || got > 100 {
		t.Fatalf("p95=%v, want a value in [90,100]", got)
	}
	if got := Percentile(nil, 95); got != 0 {
		t.Fatalf("empty p95=%v, want 0", got)
	}
	// Input order must not affect the result.
	shuffled := []float64{80, 10, 60, 30, 100, 20, 90, 50, 40, 70}
	if Percentile(values, 50) != Percentile(shuffled, 50) {
		t.Fatal("percentile must be order-independent")
	}
}

func TestEvaluateCleanPasses(t *testing.T) {
	r := &Report{Frames: 600, RecoveredPct: 100, SyncMaxMillis: 1, LatencyP95Ms: 5}
	verdict, reasons := r.Evaluate(DefaultThresholds(), false)
	if verdict != 0 || len(reasons) != 0 {
		t.Fatalf("clean run verdict=%d reasons=%v", verdict, reasons)
	}
}

func TestEvaluateUnrecoverable(t *testing.T) {
	r := &Report{Frames: 0}
	verdict, _ := r.Evaluate(DefaultThresholds(), true)
	if verdict != 2 {
		t.Fatalf("verdict=%d, want 2", verdict)
	}
}

func TestEvaluateMetricsFail(t *testing.T) {
	r := &Report{Frames: 100, RecoveredPct: 50, SyncMaxMillis: 3, LatencyP95Ms: 5}
	verdict, reasons := r.Evaluate(DefaultThresholds(), true)
	if verdict != 3 || len(reasons) == 0 {
		t.Fatalf("verdict=%d reasons=%v, want 3 with reasons", verdict, reasons)
	}
}

func TestEvaluateSyncAndLatencyGates(t *testing.T) {
	base := func() *Report {
		return &Report{Frames: 100, RecoveredPct: 99, SyncMaxMillis: 0, LatencyP95Ms: 0}
	}
	r1 := base()
	r1.SyncMaxMillis = 21
	if v, _ := r1.Evaluate(DefaultThresholds(), true); v != 3 {
		t.Fatalf("sync gate failed, verdict=%d", v)
	}
	r2 := base()
	r2.LatencyP95Ms = 41
	if v, _ := r2.Evaluate(DefaultThresholds(), true); v != 3 {
		t.Fatalf("latency gate failed, verdict=%d", v)
	}
}
