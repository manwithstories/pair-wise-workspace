package clock_test

import (
	"testing"

	"archivearchivist/internal/clock"
	"archivearchivist/internal/model"
)

// TestDriftCorrectionDoesNotGrow checks that correcting a slow media clock
// removes the accumulated error rather than doubling it. With a perfect wall
// clock and a 120 ppm slow sender, the residual after correction must stay
// near the per-sample quantization noise at every point, not grow with time.
func TestDriftCorrectionDoesNotGrow(t *testing.T) {
	const rate = 90000
	const ppm = 120
	const samples = 6000 // 60 s at 100 samples/s
	src := make([]clock.Sample, 0, samples)
	for i := 0; i < samples; i++ {
		ticks := (int64(i)*900*(1_000_000-ppm) + 500_000) / 1_000_000
		src = append(src, clock.Sample{
			Seq: uint32(i + 1), Orig: ticks,
			Arrival: int64(i) * 10_000, HasArr: true,
		})
	}
	res := clock.Unwrap(src, clock.Options{ClockRate: rate})
	if res.MaxSkewUS > 2_000 {
		t.Fatalf("residual skew grew to %d us over 60 s", res.MaxSkewUS)
	}
	// The correction must lengthen media time toward wall clock: the raw tick
	// span converts to about 60 s minus 120 ppm, and correction adds it back.
	span := src[samples-1].Orig - src[0].Orig
	uncorrected := clock.TicksToUS(span, rate, model.NewRat(0, 1))
	corrected := clock.TicksToUS(span, rate, res.DriftPPM)
	if corrected <= uncorrected {
		t.Fatalf("correction shortened a slow clock: corrected=%d uncorrected=%d", corrected, uncorrected)
	}
	if got, want := corrected-uncorrected, int64(60_000_000*ppm/1_000_000); abs64(got-want) > 5_000 {
		t.Fatalf("correction = %d us, want about %d", got, want)
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
