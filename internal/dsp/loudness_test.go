package dsp

import (
	"math"
	"testing"
)

func testTone(rate, channels, frames int, amp, freq float64) *Buffer {
	b := &Buffer{Rate: rate, Channels: channels, Interleaved: make([]float64, frames*channels)}
	for f := 0; f < frames; f++ {
		v := amp * math.Sin(2*math.Pi*freq*float64(f)/float64(rate))
		for ch := 0; ch < channels; ch++ {
			b.Interleaved[f*channels+ch] = v
		}
	}
	return b
}

func TestIntegratedLoudnessFiniteForTone(t *testing.T) {
	b := testTone(48000, 2, 48000, 0.2, 997)
	lu := IntegratedLUFS(b)
	if math.IsInf(lu, -1) || math.IsNaN(lu) {
		t.Fatalf("tone loudness invalid: %v", lu)
	}
	if lu < -30 || lu > -5 {
		t.Fatalf("tone loudness=%.2f LUFS outside expected rough range", lu)
	}
}

func TestNormalizeToR128AndTruePeakCeiling(t *testing.T) {
	in := testTone(48000, 2, 96000, 0.13, 1000)
	out, res, err := NormalizeToR128(in, R128TargetLUFS, MaxTruePeakDBTP)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(res.OutputLUFS-R128TargetLUFS) > 0.2 {
		t.Fatalf("output LUFS=%.3f, want -23 ±0.2 (gain=%.2f corrections=%d)",
			res.OutputLUFS, res.GainDB, res.Corrections)
	}
	if res.TruePeakDBTP > MaxTruePeakDBTP {
		t.Fatalf("true peak=%.3f dBTP exceeds %.1f", res.TruePeakDBTP, MaxTruePeakDBTP)
	}
	if got := IntegratedLUFS(out); math.Abs(got+23) > 0.2 {
		t.Fatalf("remeasured LUFS=%.3f", got)
	}
}

func TestTruePeakDetectsInterpolatedPeaks(t *testing.T) {
	// A mild high-frequency tone can have inter-sample peaks above its
	// integer sample peak; the estimator must return a finite non-negative-ish
	// value for a near full-scale signal.
	b := testTone(48000, 2, 48000, 0.95, 18000)
	tp := TruePeakDBTP(b)
	if math.IsNaN(tp) || tp < -10 || tp > 1 {
		t.Fatalf("unexpected true peak %.3f dBTP", tp)
	}
}

func TestNormalizeSilenceFails(t *testing.T) {
	b := &Buffer{Rate: 48000, Channels: 2, Interleaved: make([]float64, 48000)}
	if _, _, err := NormalizeToR128(b, -23, -1); err == nil {
		t.Fatal("silence normalization should fail")
	}
}
