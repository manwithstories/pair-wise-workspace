package loudness

import (
	"math"
	"testing"
)

func TestAbsoluteAndRelativeGate(t *testing.T) {
	blocks := []float64{
		energyForLUFS(-23), energyForLUFS(-23),
		energyForLUFS(-50), energyForLUFS(-80),
	}
	got, gated, reason := gateBlocks(blocks)
	if reason != "" {
		t.Fatalf("reason = %s", reason)
	}
	if gated != 2 {
		t.Fatalf("gated blocks = %d, want 2", gated)
	}
	if math.Abs(got-(-23)) > 1e-9 {
		t.Fatalf("integrated = %.9f, want -23", got)
	}
}

func TestGateReasons(t *testing.T) {
	if got, n, reason := gateBlocks([]float64{0, 0, 0}); !math.IsInf(got, -1) || n != 0 || reason != ReasonSilent {
		t.Fatalf("silence: got=%v n=%d reason=%s", got, n, reason)
	}
	tooQuiet := []float64{energyForLUFS(-80), energyForLUFS(-75)}
	if got, n, reason := gateBlocks(tooQuiet); !math.IsInf(got, -1) || n != 0 || reason != ReasonBelowAbsoluteGate {
		t.Fatalf("below absolute: got=%v n=%d reason=%s", got, n, reason)
	}
}

func TestAnalyzerTwelveSeconds(t *testing.T) {
	const fs = 48000
	a, err := NewAnalyzer(fs, 1, []float64{1})
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]float64, 1000)
	for i := range chunk {
		chunk[i] = 0.1
	}
	for left := 12 * fs; left > 0; {
		n := len(chunk)
		if left < n {
			n = left
		}
		if err := a.Add(chunk[:n], n); err != nil {
			t.Fatal(err)
		}
		left -= n
	}
	m := a.Finish()
	if m.GatedBlocks != 117 {
		t.Fatalf("gated blocks = %d, want 117", m.GatedBlocks)
	}
	want := loudnessFromEnergy(0.01)
	if math.Abs(m.IntegratedLUFS-want) > 1e-9 {
		t.Fatalf("integrated = %.9f, want %.9f", m.IntegratedLUFS, want)
	}
	if math.Abs(m.ShortTermMaxLUFS-want) > 1e-9 || math.Abs(m.MomentaryMaxLUFS-want) > 1e-9 {
		t.Fatalf("short/momentary = %.9f/%.9f", m.ShortTermMaxLUFS, m.MomentaryMaxLUFS)
	}
}

func TestAnalyzerTooShortAndSilent(t *testing.T) {
	a, _ := NewAnalyzer(48000, 1, []float64{1})
	if err := a.Add(make([]float64, 48000/4), 48000/4); err != nil {
		t.Fatal(err)
	}
	if m := a.Finish(); m.Reason != ReasonTooShort || m.GatedBlocks != 0 {
		t.Fatalf("too short metrics: %+v", m)
	}
	a, _ = NewAnalyzer(48000, 1, []float64{1})
	if err := a.Add(make([]float64, 48000), 48000); err != nil {
		t.Fatal(err)
	}
	if m := a.Finish(); m.Reason != ReasonSilent || !math.IsInf(m.IntegratedLUFS, -1) {
		t.Fatalf("silent metrics: %+v", m)
	}
}

func energyForLUFS(lufs float64) float64 {
	return math.Pow(10, (lufs+0.691)/10)
}
