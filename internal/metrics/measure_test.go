package metrics

import (
	"image"
	"image/color"
	"math"
	"testing"
)

func uniform(c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for i := range img.Pix {
		img.Pix[i] = 0
	}
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func TestMSEAndPSNR(t *testing.T) {
	a := uniform(color.RGBA{R: 100, G: 100, B: 100, A: 255})
	if m := MSE(a, a); m != 0 {
		t.Fatalf("self MSE=%v", m)
	}
	if p := PSNR(0); !math.IsInf(p, 1) {
		t.Fatalf("PSNR(0)=%v", p)
	}
	b := uniform(color.RGBA{R: 110, G: 100, B: 100, A: 255})
	if m := MSE(a, b); m != 100.0/3.0 {
		t.Fatalf("MSE=%v want %v", m, 100.0/3.0)
	}
	if p := PSNR(100.0 / 3.0); p < 30 || p > 40 {
		t.Fatalf("PSNR=%v out of expected range", p)
	}
}

func TestMedian(t *testing.T) {
	if got := Median([]float64{3, 1, 2}); got != 2 {
		t.Fatalf("median=%v", got)
	}
	if got := Median([]float64{4, 1, 2, 3}); got != 2.5 {
		t.Fatalf("median=%v", got)
	}
	if got := Median(nil); got != 0 {
		t.Fatalf("median=%v", got)
	}
}

func TestBitrateDeviationAlign(t *testing.T) {
	if b := Bitrate(1000, 2); b != 4000 {
		t.Fatalf("bitrate=%v", b)
	}
	if d := DeviationPct(1080, 1000); d != 8 {
		t.Fatalf("deviation=%v", d)
	}
	got := []int64{0, 3750, 7503}
	want := []int64{0, 3750, 7500}
	ms := AlignMaxMs(got, want, 1.0/90000)
	if math.Abs(ms-3.0/90) > 1e-9 {
		t.Fatalf("align=%v want %v", ms, 3.0/90)
	}
}
