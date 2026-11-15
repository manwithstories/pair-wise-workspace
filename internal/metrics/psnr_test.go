package metrics

import (
	"math"
	"testing"

	"rcctl/internal/source"
)

func TestFramePSNR(t *testing.T) {
	a := source.Frame{Width: 8, Height: 8, Y: make([]byte, 64)}
	b := source.Frame{Width: 8, Height: 8, Y: make([]byte, 64)}
	for i := range b.Y {
		b.Y[i] = 10
	}
	want := 10 * math.Log10(255.0*255.0/100.0)
	if got := FramePSNR(a, b); math.Abs(got-want) > 1e-9 {
		t.Fatalf("PSNR %.6f, want %.6f", got, want)
	}
	if got := FramePSNR(a, a.Clone()); got != 100 {
		t.Fatalf("identical PSNR %.3f, want 100", got)
	}
}

func TestAggregate(t *testing.T) {
	mean, std := Aggregate([]float64{34, 36})
	if mean != 35 || std != 1 {
		t.Fatalf("got mean %.2f std %.2f", mean, std)
	}
}
