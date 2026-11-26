package av

import (
	"math"
	"testing"

	"mediad/internal/media"
)

// TestDefaultMatrixKnownLayouts pins the explicit mixing rules.
func TestDefaultMatrixKnownLayouts(t *testing.T) {
	mono, err := DefaultMatrix("mono", 1)
	if err != nil {
		t.Fatal(err)
	}
	if mono[0][0] != 1 || mono[1][0] != 1 {
		t.Fatalf("mono should be centred: %+v", mono)
	}
	stereo, err := DefaultMatrix("stereo", 2)
	if err != nil {
		t.Fatal(err)
	}
	if stereo[0][0] != 1 || stereo[0][1] != 0 || stereo[1][0] != 0 || stereo[1][1] != 1 {
		t.Fatalf("stereo passthrough wrong: %+v", stereo)
	}
	if _, err := DefaultMatrix("mystery", 3); err == nil {
		t.Fatal("unknown layout must be rejected")
	}
}

// TestValidateMatrixRejectsBadShape guards the coefficient-count check.
func TestValidateMatrixRejectsBadShape(t *testing.T) {
	bad := MixMatrix{{1, 0}, {0}}
	if err := ValidateMatrix(bad, 2); err == nil {
		t.Fatal("short row accepted")
	}
	if err := ValidateMatrix(MixMatrix{{1, 0}, {0, 1}}, 2); err != nil {
		t.Fatalf("good matrix rejected: %v", err)
	}
}

// TestSpanExactAt48k checks sample-exact cut mapping for common time bases.
func TestSpanExactAt48k(t *testing.T) {
	cases := []struct {
		tb   string
		from int64
		to   int64
		want int
	}{
		{"1/48000", 0, 48000, 48000},
		{"1/1000", 0, 1000, 48000},
		{"1/90000", 0, 90000, 48000},
		{"1/44100", 0, 44100, 48000},
	}
	for _, c := range cases {
		tb := media.MustParseRational(c.tb)
		got, err := SpanSamples(tb, c.from, c.to)
		if err != nil {
			t.Fatalf("%s: %v", c.tb, err)
		}
		if got != c.want {
			t.Fatalf("%s: got %d want %d", c.tb, got, c.want)
		}
		align, err := SpanAlignmentUS(tb, c.from, c.to)
		if err != nil {
			t.Fatal(err)
		}
		if align > 21 {
			t.Fatalf("%s alignment %.3f µs exceeds one sample", c.tb, align)
		}
	}
}

// TestNormalizeGainRespectsTruePeak ensures the normaliser never pushes the
// true peak above -1 dBTP even when the loudness target would require it.
func TestNormalizeGainRespectsTruePeak(t *testing.T) {
	rep := LoudnessReport{IntegratedLUFS: -30, TruePeakDBTP: -1.0, SamplePeakDBFS: -1.0, Frames: 48000}
	gain := NormalizeGain(rep)
	if 20*math.Log10(gain)+rep.TruePeakDBTP > MaxTruePeak+1e-6 {
		t.Fatalf("gain %.3f would exceed true peak: %.2f dBTP", gain, 20*math.Log10(gain)+rep.TruePeakDBTP)
	}
}

// TestNormalizeGainLeavesSilenceAlone asserts silent programmes are never
// amplified.
func TestNormalizeGainLeavesSilenceAlone(t *testing.T) {
	rep := LoudnessReport{IntegratedLUFS: -70, TruePeakDBTP: -144, SamplePeakDBFS: -144, Frames: 48000}
	if g := NormalizeGain(rep); g != 1 {
		t.Fatalf("silence gained: %f", g)
	}
}

// TestMeasureToneHitsTarget checks the loudness meter against a known tone.
func TestMeasureToneHitsTarget(t *testing.T) {
	const frames = 48000 * 4
	samples := make([]float32, frames*2)
	for i := 0; i < frames; i++ {
		v := float32(0.25 * math.Sin(2*math.Pi*1000*float64(i)/48000))
		samples[i*2] = v
		samples[i*2+1] = v
	}
	rep := MeasureInterleaved(samples)
	if rep.ClippedSamples != 0 {
		t.Fatalf("clipped=%d", rep.ClippedSamples)
	}
	if rep.IntegratedLUFS > -12 || rep.IntegratedLUFS < -20 {
		t.Fatalf("lufs=%.2f", rep.IntegratedLUFS)
	}
	if rep.TruePeakDBTP < -20 || rep.TruePeakDBTP > -5 {
		t.Fatalf("tp=%.2f", rep.TruePeakDBTP)
	}
}
