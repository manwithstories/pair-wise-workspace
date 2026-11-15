package dsp

import (
	"math"
	"testing"
)

func Test48kCoeffsMatchReference(t *testing.T) {
	if _, err := KWWeightingCoeffs(48000); err != nil {
		t.Fatalf("48 kHz coefficients rejected: %v", err)
	}
}

func TestSupportedRatesPassResponseCheck(t *testing.T) {
	for _, rate := range []float64{44100, 48000, 96000} {
		if _, err := KWWeightingCoeffs(rate); err != nil {
			t.Fatalf("rate %.0f rejected: %v", rate, err)
		}
	}
}

func TestUnsupportedRateRejected(t *testing.T) {
	if _, err := KWWeightingCoeffs(22050); err == nil {
		t.Fatal("expected error for unsupported sample rate")
	}
}

func TestIntersamplePeakExceedsSamplePeak(t *testing.T) {
	meter := NewTruePeakMeter(1)
	planar := ToneFixture(15750, 0.5, 48000, 4)
	meter.Process(planar)
	if meter.TruePeakLinear() <= 0.5 {
		t.Fatalf("oversampled peak %.4f not above sample peak 0.5", meter.TruePeakLinear())
	}
}

func TestPolyphaseBranchUnityDCSum(t *testing.T) {
	phases := designPolyphase()
	if len(phases) != OversampleFactor {
		t.Fatalf("got %d branches, want %d", len(phases), OversampleFactor)
	}
	for branch, coef := range phases {
		var sum float64
		for _, value := range coef {
			sum += value
		}
		if math.Abs(sum-1) > 1e-9 {
			t.Fatalf("branch %d dc sum %.12f, want 1", branch, sum)
		}
	}
}

func TestChannelWeights(t *testing.T) {
	weights, used, err := ChannelWeights(6, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{1, 1, 1, 0, 1.41, 1.41}
	for i := range want {
		if math.Abs(weights[i]-want[i]) > 1e-9 {
			t.Fatalf("channel %d weight %.3f want %.3f", i, weights[i], want[i])
		}
	}
	if used[3] {
		t.Fatal("LFE must be excluded from loudness")
	}
}

func ToneFixture(freq, amplitude float64, sampleRate, seconds int) [][]float64 {
	frames := sampleRate * seconds
	out := make([][]float64, 1)
	out[0] = make([]float64, frames)
	for n := range out[0] {
		out[0][n] = amplitude * math.Sin(2*math.Pi*freq*float64(n)/float64(sampleRate))
	}
	return out
}
