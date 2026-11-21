package render

import (
	"image"
	"image/color"
	"testing"

	"mediad/internal/metadata"
	"mediad/internal/timeline"
)

func testLayer(key string, index int64) timeline.Layer {
	return timeline.Layer{
		TrackKey: key, SourceIndex: index, Rotation: 0,
		Color: metadata.NewColor(metadata.PrimariesBT709, metadata.TransferBT709,
			metadata.MatrixBT709, false, metadata.SourceColr),
		W: 64, H: 36,
	}
}

func TestSourceIsDeterministic(t *testing.T) {
	e := NewEngine(42)
	a := e.Source("v0", 3, 64, 36)
	b := e.Source("v0", 3, 64, 36)
	if HashImage(a) != HashImage(b) {
		t.Fatal("same input produced different source")
	}
	c := e.Source("v0", 4, 64, 36)
	if HashImage(a) == HashImage(c) {
		t.Fatal("different sample index produced identical source")
	}
	d := e.Source("v1", 3, 64, 36)
	if HashImage(a) == HashImage(d) {
		t.Fatal("different track produced identical source")
	}
}

func TestRotateDimensionsAndInverse(t *testing.T) {
	src := testSource(8, 4)
	rot := Rotate(src, 90)
	if rot.Bounds().Dx() != 4 || rot.Bounds().Dy() != 8 {
		t.Fatalf("90 dims = %dx%d", rot.Bounds().Dx(), rot.Bounds().Dy())
	}
	back := Rotate(rot, 270)
	if HashImage(back) != HashImage(src) {
		t.Fatal("rotate 90 then 270 is not identity")
	}
	twice := Rotate(Rotate(src, 90), 90)
	if HashImage(twice) != HashImage(Rotate(src, 180)) {
		t.Fatal("90+90 != 180")
	}
}

func TestCropClampsToBounds(t *testing.T) {
	src := testSource(10, 6)
	out := Crop(src, &metadata.Crop{X: 2, Y: 1, W: 100, H: 100})
	if out.Bounds().Dx() != 8 || out.Bounds().Dy() != 5 {
		t.Fatalf("crop dims = %dx%d", out.Bounds().Dx(), out.Bounds().Dy())
	}
}

func TestApplyColorIsDeterministicAndChangesPixels(t *testing.T) {
	src := testSource(16, 16)
	plain := ApplyColor(src, metadata.DefaultColor)
	if HashImage(plain) != HashImage(src) {
		t.Fatal("default color transform changed pixels")
	}
	pq := metadata.NewColor(metadata.PrimariesBT2020, metadata.TransferPQ,
		metadata.MatrixBT2020, false, metadata.SourceColr)
	a := ApplyColor(src, pq)
	b := ApplyColor(src, pq)
	if HashImage(a) != HashImage(b) {
		t.Fatal("color transform not deterministic")
	}
	if HashImage(a) == HashImage(src) {
		t.Fatal("PQ/BT.2020 transform had no effect")
	}
}

func TestRenderFrameBlockedIsBlackAndDeterministic(t *testing.T) {
	e := NewEngine(42)
	f := timeline.Frame{Index: 0, Start: 0, End: 3000, Width: 64, Height: 36,
		Blocked: true, BlockingDep: "transport:gap", PlanHash: "plan", Color: metadata.DefaultColor}
	a, err := e.RenderFrame(f)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := e.RenderFrame(f)
	if a.Hash != b.Hash {
		t.Fatal("blocked frame not deterministic")
	}
	if a.BlockingDep != "transport:gap" {
		t.Fatal("blocking dep lost")
	}
	if a.Image.RGBAAt(0, 0) != (color.RGBA{A: 255}) {
		t.Fatal("blocked frame is not black")
	}
}

func TestRenderFrameLayersAndHash(t *testing.T) {
	e := NewEngine(42)
	f := timeline.Frame{Index: 1, Start: 3000, End: 6000, Width: 64, Height: 36,
		PlanHash: "plan-1", Color: metadata.DefaultColor,
		Layers: []timeline.Layer{testLayer("v0", 0)}}
	a, err := e.RenderFrame(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.LayerHashes) != 1 || a.Hash == "" {
		t.Fatalf("rendered = %+v", a)
	}
	b, _ := e.RenderFrame(f)
	if a.Hash != b.Hash {
		t.Fatal("frame hash not reproducible")
	}
	// A different plan must yield a different frame hash.
	f.PlanHash = "plan-2"
	c, _ := e.RenderFrame(f)
	if c.Hash == a.Hash {
		t.Fatal("frame hash ignores plan")
	}
}

func TestPSNRAndSSIM(t *testing.T) {
	a := testSource(32, 32)
	if got := PSNR(a, a); got < 99 {
		t.Fatalf("self PSNR = %f", got)
	}
	if got := SSIM(a, a); got < 0.999 {
		t.Fatalf("self SSIM = %f", got)
	}
	b := testSource(32, 32)
	b.SetRGBA(0, 0, color.RGBA{R: 255, A: 255})
	if got := PSNR(a, b); got >= 100 {
		t.Fatalf("different images PSNR = %f", got)
	}
	if got := SSIM(a, b); got >= 1 {
		t.Fatalf("different images SSIM = %f", got)
	}
	// Mismatched dimensions are a hard zero, never a panic.
	c := testSource(16, 16)
	if PSNR(a, c) != 0 || SSIM(a, c) != 0 {
		t.Fatal("mismatched sizes must score zero")
	}
}

func testSource(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x * 3), G: uint8(y * 5), B: uint8(x + y), A: 255})
		}
	}
	return img
}
