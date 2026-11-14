package quality

import (
	"math"
	"testing"
)

func TestFramePSNRIdentical(t *testing.T) {
	a := []byte{1, 2, 3, 4}
	if got := FramePSNR(a, a); got != MaxPSNR {
		t.Fatalf("identical frames: got %f want %f", got, MaxPSNR)
	}
}

func TestFramePSNRKnownMSE(t *testing.T) {
	a := []byte{0, 0, 0, 0}
	b := []byte{10, 10, 10, 10} // mse = 100
	want := 10 * math.Log10(255.0*255.0/100.0)
	if got := FramePSNR(a, b); math.Abs(got-want) > 1e-9 {
		t.Fatalf("got %f want %f", got, want)
	}
}

func TestMeanStdSpread(t *testing.T) {
	m, s := MeanStd([]float64{2, 4, 4, 4, 5, 5, 7, 9})
	if m != 5 {
		t.Fatalf("mean: got %f", m)
	}
	if math.Abs(s-2) > 1e-9 {
		t.Fatalf("std: got %f", s)
	}
	if got := Spread([]float64{3, 9, 1}); got != 8 {
		t.Fatalf("spread: got %f", got)
	}
	if m, s := MeanStd(nil); m != 0 || s != 0 {
		t.Fatal("empty input should give zeros")
	}
}
