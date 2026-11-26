package render

import (
	"image"
	"image/color"
	"math"
	"testing"
)

func solid(w, h int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

func TestPSNRIdentical(t *testing.T) {
	a := solid(8, 8, color.NRGBA{10, 20, 30, 255})
	p, err := PSNR(a, a)
	if err != nil {
		t.Fatal(err)
	}
	if p != MaxPSNRDB {
		t.Fatalf("identical PSNR = %v, want %v", p, MaxPSNRDB)
	}
}

func TestPSNRKnownValue(t *testing.T) {
	a := solid(8, 8, color.NRGBA{100, 100, 100, 255})
	b := solid(8, 8, color.NRGBA{110, 100, 100, 255})
	p, err := PSNR(a, b)
	if err != nil {
		t.Fatal(err)
	}
	// MSE = 100/3（仅 R 通道差 10）
	want := 10 * math.Log10(255*255/(100.0/3.0))
	if math.Abs(p-want) > 0.01 {
		t.Fatalf("PSNR = %v, want %v", p, want)
	}
}

func TestPSNRSizeMismatch(t *testing.T) {
	a := solid(8, 8, color.NRGBA{})
	b := solid(4, 8, color.NRGBA{})
	if _, err := PSNR(a, b); err == nil {
		t.Fatal("size mismatch accepted")
	}
}

func TestPSNRStats(t *testing.T) {
	a := []image.Image{solid(8, 8, color.NRGBA{50, 50, 50, 255}), solid(8, 8, color.NRGBA{60, 60, 60, 255})}
	mean, min, err := PSNRStats(a, a)
	if err != nil {
		t.Fatal(err)
	}
	if mean != MaxPSNRDB || min != MaxPSNRDB {
		t.Fatalf("stats = %v/%v, want capped max", mean, min)
	}
	if _, _, err := PSNRStats(a, a[:1]); err == nil {
		t.Fatal("count mismatch accepted")
	}
	if _, _, err := PSNRStats(nil, nil); err == nil {
		t.Fatal("empty accepted")
	}
}

func TestBlackFrame(t *testing.T) {
	b := BlackFrame(4, 4)
	r, g, bl, a := b.At(2, 2).RGBA()
	if r != 0 || g != 0 || bl != 0 || a != 0xffff {
		t.Fatalf("black frame pixel = %v,%v,%v,%v", r, g, bl, a)
	}
}
