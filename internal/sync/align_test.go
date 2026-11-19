package sync

import (
	"math"
	"testing"

	"avpipe/internal/clock"
	"avpipe/internal/fault"
)

func TestAlignNoDriftNoJitter(t *testing.T) {
	res, err := Align(Config{Seed: 1}, 200, clock.FromMillis(20))
	if err != nil {
		t.Fatal(err)
	}
	if res.P95Ms != 0 || res.MaxMs != 0 {
		t.Fatalf("clean run has error: %+v", res)
	}
	if math.Abs(res.DriftEstPPM) > 1e-6 {
		t.Fatalf("drift estimate %v", res.DriftEstPPM)
	}
}

func TestAlignNormalJitterUnder20ms(t *testing.T) {
	res, err := Align(Config{Seed: 42, JitterMs: 12}, 500, clock.FromMillis(20))
	if err != nil {
		t.Fatal(err)
	}
	if res.P95Ms >= 20 {
		t.Fatalf("p95 %v exceeds 20ms at 12ms jitter", res.P95Ms)
	}
}

func TestAlignDriftBoundaryUnder80ms(t *testing.T) {
	// 500 intervals of 20ms = 10s. 12,000ppm accumulates to 120ms of raw
	// offset; the sliding estimator must leave a residual under 80ms while
	// the raw error is clearly above the threshold.
	res, err := Align(Config{Seed: 7, JitterMs: 30, DriftPPM: 12000, Recoveries: 4},
		500, clock.FromMillis(20))
	if err != nil {
		t.Fatal(err)
	}
	if res.MaxBeforeMs < 100 {
		t.Fatalf("raw error %v too small to be a boundary case", res.MaxBeforeMs)
	}
	if res.MaxMs >= 80 {
		t.Fatalf("corrected max %v exceeds 80ms", res.MaxMs)
	}
	// With 30ms uniform jitter over a 10s span, the least-squares slope
	// carries a few-hundred-ppm statistical error; require 5% relative
	// accuracy rather than an unrealistic exact match.
	if math.Abs(res.DriftEstPPM-12000) > 600 {
		t.Fatalf("drift estimate %v far from 12000", res.DriftEstPPM)
	}
}

func TestDriftEstimateAccuracy(t *testing.T) {
	pairs := make([]Pair, 0, 100)
	for i := 0; i < 100; i++ {
		tick := clock.FromMillis(float64(i) * 20)
		drift := clock.Tick(float64(tick) * 2500 / 1e6)
		pairs = append(pairs, Pair{Tick: tick, Audio: tick, Video: tick + drift})
	}
	est := EstimateDrift(pairs)
	if math.Abs(est-2500) > 1 {
		t.Fatalf("estimate %v want 2500", est)
	}
}

func TestApplyResyncReducesError(t *testing.T) {
	pairs := make([]Pair, 0, 100)
	for i := 0; i < 100; i++ {
		tick := clock.FromMillis(float64(i) * 20)
		drift := clock.Tick(float64(tick) * 4000 / 1e6)
		pairs = append(pairs, Pair{Tick: tick, Audio: tick, Video: tick + drift,
			Error: drift.Millis()})
	}
	est := EstimateDrift(pairs)
	fixed, n := ApplyResync(pairs, est)
	// Frame 0 has zero elapsed time, so its correction is legitimately zero.
	if n != len(pairs)-1 {
		t.Fatalf("fixed %d of %d", n, len(pairs))
	}
	var maxBefore, maxAfter float64
	for i := range pairs {
		maxBefore = math.Max(maxBefore, math.Abs(pairs[i].Error))
		maxAfter = math.Max(maxAfter, math.Abs(fixed[i].Error))
	}
	if maxAfter >= maxBefore {
		t.Fatalf("resync did not improve: %v -> %v", maxBefore, maxAfter)
	}
	// Residual is only 90 kHz tick quantization (one tick = 0.0111 ms).
	if maxAfter > 0.02 {
		t.Fatalf("residual %v not near zero", maxAfter)
	}
}

func TestAlignDeterministic(t *testing.T) {
	a, _ := Align(Config{Seed: 11, JitterMs: 35, DriftPPM: 500}, 300, clock.FromMillis(20))
	b, _ := Align(Config{Seed: 11, JitterMs: 35, DriftPPM: 500}, 300, clock.FromMillis(20))
	if a.P95Ms != b.P95Ms || a.MaxMs != b.MaxMs || len(a.Pairs) != len(b.Pairs) {
		t.Fatal("alignment nondeterministic")
	}
	for i := range a.Pairs {
		if a.Pairs[i] != b.Pairs[i] {
			t.Fatalf("pair %d differs", i)
		}
	}
}

func TestAlignRejectsBadInput(t *testing.T) {
	if _, err := Align(Config{}, 0, clock.FromMillis(20)); err == nil {
		t.Fatal("zero intervals accepted")
	}
	_, err := Align(Config{JitterMs: -5}, 10, clock.FromMillis(20))
	if e := fault.Root(err); e == nil || e.Code != fault.CodeInvalidArgument {
		t.Fatalf("want invalid argument, got %v", err)
	}
}

func TestJitterStats(t *testing.T) {
	pairs := []Pair{
		{Tick: 0, Arrival: clock.FromMillis(3)},
		{Tick: clock.FromMillis(10), Arrival: clock.FromMillis(15)},
	}
	mean, max := JitterStats(pairs)
	if max < 4.99 || max > 5.01 {
		t.Fatalf("max jitter %v", max)
	}
	if mean < 3.99 || mean > 4.01 {
		t.Fatalf("mean jitter %v", mean)
	}
}
