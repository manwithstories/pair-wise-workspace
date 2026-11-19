package clock_test

import (
	"testing"

	"archivearchivist/internal/clock"
	"archivearchivist/internal/model"
)

func TestUnwrapSingleWrap(t *testing.T) {
	mod := int64(model.Modulus33)
	start := mod - 6000 // about 66 ms before the boundary
	samples := []clock.Sample{
		{Seq: 1, Orig: start & (mod - 1)},
		{Seq: 2, Orig: (start + 1000) & (mod - 1)},
		{Seq: 3, Orig: (start + 3000) & (mod - 1)},
		{Seq: 4, Orig: (start + 6000) & (mod - 1)}, // wrapped to 0
		{Seq: 5, Orig: (start + 9000) & (mod - 1)},
	}
	res := clock.Unwrap(samples, clock.Options{})
	if res.Wraps != 1 {
		t.Fatalf("wraps = %d, want 1", res.Wraps)
	}
	if got, want := res.Points[3].Norm, start+6000; got != want {
		t.Fatalf("normalized = %d, want %d", got, want)
	}
	if !res.Points[3].Wrapped || res.Points[2].Wrapped {
		t.Fatalf("wrap flags wrong: %+v", res.Points)
	}
	if !res.Faults.Has(model.CodeTimestampWrap) {
		t.Fatalf("missing wrap fault")
	}
	if got := res.Points[4].Norm - res.Points[0].Norm; got != 9000 {
		t.Fatalf("duration = %d ticks, want 9000", got)
	}
}

func TestUnwrapForwardJumpAcrossBoundary(t *testing.T) {
	// A packet delivered out of order from just before the wrap arrives after
	// the first post-wrap packet.
	mod := int64(model.Modulus33)
	samples := []clock.Sample{
		{Seq: 1, Orig: mod - 100},
		{Seq: 3, Orig: 100},
		{Seq: 2, Orig: mod - 50},
		{Seq: 4, Orig: 200},
	}
	res := clock.Unwrap(samples, clock.Options{})
	if res.Wraps != 1 {
		t.Fatalf("wraps = %d, want 1", res.Wraps)
	}
	if res.Points[2].Epoch != 0 {
		t.Fatalf("late pre-wrap packet epoch = %d, want 0", res.Points[2].Epoch)
	}
	if res.Points[2].Norm >= res.Points[1].Norm {
		t.Fatalf("pre-wrap packet should normalize before post-wrap packet")
	}
}

func TestUnwrapKeepsOriginalAndNormalized(t *testing.T) {
	mod := int64(model.Modulus33)
	samples := []clock.Sample{
		{Seq: 1, Orig: mod - 10},
		{Seq: 2, Orig: 10},
	}
	res := clock.Unwrap(samples, clock.Options{})
	if res.Points[1].Orig != 10 {
		t.Fatalf("original timestamp not preserved: %+v", res.Points[1])
	}
	if res.Points[1].Norm != mod+10 {
		t.Fatalf("normalized = %d, want %d", res.Points[1].Norm, mod+10)
	}
}

func TestDriftEstimateWithinOnePPM(t *testing.T) {
	// 120 ppm slow clock at 90 kHz, above the 80 ppm review threshold,
	// sampled every 10 ms for one second.
	const rate = 90000
	const ppm = 120
	samples := make([]clock.Sample, 0, 100)
	for i := 0; i < 100; i++ {
		// Media ticks accumulate ppm slower than wall clock: at 90 kHz the
		// nominal step is 900 ticks per 10 ms. Round rather than truncate so
		// the fixture does not inject a systematic bias of its own.
		ticks := (int64(i)*900*(1_000_000-ppm) + 500_000) / 1_000_000
		arrival := int64(i) * 10000
		samples = append(samples, clock.Sample{
			Seq: uint32(i + 1), Orig: ticks,
			Arrival: arrival, HasArr: true,
		})
	}
	res := clock.Unwrap(samples, clock.Options{ClockRate: rate})
	if !res.DriftExceeded {
		t.Fatalf("expected drift threshold breach, got ppm=%s", res.DriftPPM)
	}
	// Quantization in the fixture is under a tick per sample; allow 2 ppm.
	dev := res.DriftPPM.Num - int64(ppm)*res.DriftPPM.Den
	if dev < 0 {
		dev = -dev
	}
	if dev > 2*res.DriftPPM.Den {
		t.Fatalf("drift = %s ppm, want %d ppm within 2", res.DriftPPM, ppm)
	}
}

func TestDriftReviewThreshold(t *testing.T) {
	// 20 ppm is below the 80 ppm review threshold.
	samples := make([]clock.Sample, 0, 60)
	for i := 0; i < 60; i++ {
		samples = append(samples, clock.Sample{
			Seq: uint32(i + 1), Orig: int64(i * 900),
			Arrival: int64(i) * 10000, HasArr: true,
		})
	}
	res := clock.Unwrap(samples, clock.Options{})
	if res.DriftExceeded {
		t.Fatalf("20 ppm drift should not exceed threshold, got %s", res.DriftPPM)
	}
}

func TestWrongModulusMultipleWraps(t *testing.T) {
	mod := int64(1 << 16)
	samples := []clock.Sample{
		{Seq: 1, Orig: 60000},
		{Seq: 2, Orig: 1000},
		{Seq: 3, Orig: 4000},
	}
	res := clock.Unwrap(samples, clock.Options{Modulus: mod})
	if res.Wraps != 1 {
		t.Fatalf("wraps = %d, want 1", res.Wraps)
	}
	if res.Points[2].Norm != mod+4000 {
		t.Fatalf("norm = %d, want %d", res.Points[2].Norm, mod+4000)
	}
}

func TestEmptyInput(t *testing.T) {
	res := clock.Unwrap(nil, clock.Options{})
	if len(res.Points) != 0 || res.Wraps != 0 || len(res.Faults) != 0 {
		t.Fatalf("unexpected result for empty input: %+v", res)
	}
}

func TestRegressionsCounted(t *testing.T) {
	samples := []clock.Sample{
		{Seq: 1, Orig: 100000},
		{Seq: 2, Orig: 1000000},
		{Seq: 3, Orig: 200000}, // 8 s backwards, below wrap guard
	}
	res := clock.Unwrap(samples, clock.Options{WrapGuard: model.Modulus33 / 2})
	if res.Regressions != 1 {
		t.Fatalf("regressions = %d, want 1", res.Regressions)
	}
}
