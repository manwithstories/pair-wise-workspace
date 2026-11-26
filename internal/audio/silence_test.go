package audio

import (
	"math"
	"testing"
)

func toneWithSilences(durationSec float64, silences [][2]float64, sr int) []float64 {
	n := int(durationSec * float64(sr))
	out := make([]float64, n)
	for i := range out {
		t := float64(i) / float64(sr)
		silent := false
		for _, r := range silences {
			if t >= r[0] && t < r[1] {
				silent = true
				break
			}
		}
		if !silent {
			out[i] = 0.35 * math.Sin(2*math.Pi*440*t)
		}
	}
	return out
}

func TestSilenceHysteresisFindsTwoRegionsAndSuppressesShort(t *testing.T) {
	sr := 48_000
	samples := toneWithSilences(6, [][2]float64{{0.5, 1.3}, {2.5, 3.4}, {4.5, 4.56}}, sr)
	rep, err := Analyze(samples, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Regions) != 2 {
		t.Fatalf("regions=%+v shortSuppressed=%d", rep.Regions, rep.ShortSuppressed)
	}
	if rep.ShortSuppressed == 0 {
		t.Fatal("expected 60ms silence to be suppressed")
	}
	if rep.PeakAmplitude < 0.3 || rep.PeakDBFS > -8 || rep.PeakDBFS < -10 {
		t.Fatalf("unexpected peak amp=%.4f db=%.2f", rep.PeakAmplitude, rep.PeakDBFS)
	}
	if math.Abs(rep.Regions[0].DurationSec-0.8) > 0.05 {
		t.Fatalf("first duration = %.3f", rep.Regions[0].DurationSec)
	}
}

func TestBoundarySilenceIncluded(t *testing.T) {
	sr := 16_000
	samples := toneWithSilences(4, [][2]float64{{0, 0.6}, {3.3, 4}}, sr)
	rep, err := Analyze(samples, Options{SampleRate: sr, WindowMS: 20, MinSilenceMS: 200})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Regions) != 2 {
		t.Fatalf("expected start/end regions, got %+v", rep.Regions)
	}
	if rep.Regions[0].Boundary != "start" {
		t.Fatalf("first boundary = %q", rep.Regions[0].Boundary)
	}
	last := rep.Regions[len(rep.Regions)-1]
	if last.Boundary != "end" && last.Boundary != "both" {
		t.Fatalf("last boundary = %q", last.Boundary)
	}
}

func TestPCM16Conversion(t *testing.T) {
	b := []byte{0x00, 0x40, 0x00, 0x40} // two mono-ish samples ~0.5
	got := PCM16ToFloat(b, 1)
	if len(got) != 2 || got[0] <= 0.49 || got[0] >= 0.51 {
		t.Fatalf("pcm conversion = %+v", got)
	}
}
