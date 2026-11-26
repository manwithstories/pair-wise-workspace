package pipeline

import (
	"math"
	"testing"

	"mediad/internal/av"
)

func TestNormalizationConverges(t *testing.T) {
	// A quiet tonal programme should normalise to the target.
	const frames = 48000 * 8
	s := make([]float32, frames*2)
	for i := 0; i < frames; i++ {
		v := float32(0.05 * math.Sin(2*math.Pi*440*float64(i)/48000))
		s[i*2] = v
		s[i*2+1] = v
	}
	m1 := av.NewLoudnessMeter()
	m1.Push(s)
	r1 := m1.Report()
	g := av.NormalizeGain(r1)
	av.ApplyGain(s, g)
	m2 := av.NewLoudnessMeter()
	m2.Push(s)
	r2 := m2.Report()
	if math.Abs(r2.IntegratedLUFS-av.TargetLUFS) > 0.5 {
		t.Fatalf("after gain %.3f: lufs=%.2f (start %.2f)", g, r2.IntegratedLUFS, r1.IntegratedLUFS)
	}
	if r2.TruePeakDBTP > av.MaxTruePeak+0.1 {
		t.Fatalf("true peak %.2f exceeds limit", r2.TruePeakDBTP)
	}
}
