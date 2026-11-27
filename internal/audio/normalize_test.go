package audio

import (
	"math"
	"testing"
)

func sine(seconds float64, freq, amp float64) []float32 {
	n := int(seconds * InRate)
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(amp * math.Sin(2*math.Pi*freq*float64(i)/InRate))
	}
	return out
}

func TestNormalizeSpec(t *testing.T) {
	res := Normalize(sine(2, 440, 0.2), nil)
	wantFrames := 2 * InRate * 160 / 147
	if len(res.Samples) != wantFrames*2 {
		t.Fatalf("frames = %d, want %d", len(res.Samples)/2, wantFrames)
	}
	if math.Abs(res.IntegratedLUFS-TargetLUFS) > 0.5 {
		t.Fatalf("integrated_lufs = %.2f, want -18.0±0.5", res.IntegratedLUFS)
	}
	if res.TruePeakDBTP > MaxTruePeakDBTP {
		t.Fatalf("true peak = %.2f dBTP, want <= -1", res.TruePeakDBTP)
	}
}

func TestNormalizeTruePeakLimited(t *testing.T) {
	// Full-scale signal: loudness gain alone would clip above -1 dBTP.
	res := Normalize(sine(2, 1000, 0.99), nil)
	if res.TruePeakDBTP > MaxTruePeakDBTP+0.05 {
		t.Fatalf("true peak = %.2f dBTP exceeds limit", res.TruePeakDBTP)
	}
}

func TestMissingIntervalsSilenced(t *testing.T) {
	in := sine(1, 440, 0.5)
	missing := [][2]int{{10000, 14410}} // 100 ms gap
	res := Normalize(in, missing)
	if len(res.Missing) != 1 {
		t.Fatalf("missing intervals = %v", res.Missing)
	}
	start, end := res.Missing[0][0]*2, res.Missing[0][1]*2
	for i := start; i < end; i++ {
		if res.Samples[i] != 0 {
			t.Fatalf("sample %d not silenced: %v", i, res.Samples[i])
		}
	}
	// Signal must be present before and after the gap.
	if res.Samples[start-200] == 0 && res.Samples[start-100] == 0 {
		t.Fatal("signal missing before gap")
	}
	if res.Samples[end+100] == 0 && res.Samples[end+200] == 0 {
		t.Fatal("signal missing after gap")
	}
}

func TestDeterministic(t *testing.T) {
	in := sine(1, 440, 0.3)
	a := Normalize(in, [][2]int{{5000, 6000}})
	b := Normalize(in, [][2]int{{5000, 6000}})
	if a.IntegratedLUFS != b.IntegratedLUFS || len(a.Samples) != len(b.Samples) {
		t.Fatal("non-deterministic result")
	}
	for i := range a.Samples {
		if a.Samples[i] != b.Samples[i] {
			t.Fatalf("sample %d differs", i)
		}
	}
}
