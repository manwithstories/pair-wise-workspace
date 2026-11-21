package render

import (
	"testing"

	"mediad/internal/metadata"
	"mediad/internal/timeline"
)

// TestGeometryParity proves the forward compositor and the independent
// inverse-mapped reference agree pixel-for-pixel for every rotation, with and
// without a clean aperture, at full and downscaled verification resolution.
// The color is left at identity so the test isolates geometry.
func TestGeometryParity(t *testing.T) {
	col := metadata.DefaultColor
	cases := []struct {
		name string
		l    timeline.Layer
		w, h int
	}{
		{"full-rot0", timeline.Layer{TrackKey: "v0", SourceIndex: 1, Color: col,
			CodedW: 96, CodedH: 54, W: 96, H: 54}, 96, 54},
		{"full-rot90", timeline.Layer{TrackKey: "v0", SourceIndex: 1, Rotation: 90, Color: col,
			CodedW: 96, CodedH: 54, W: 54, H: 96}, 54, 96},
		{"full-rot180", timeline.Layer{TrackKey: "v0", SourceIndex: 2, Rotation: 180, Color: col,
			CodedW: 96, CodedH: 54, W: 96, H: 54}, 96, 54},
		{"full-rot270", timeline.Layer{TrackKey: "v0", SourceIndex: 2, Rotation: 270, Color: col,
			CodedW: 96, CodedH: 54, W: 54, H: 96}, 54, 96},
		{"crop-rot0", timeline.Layer{TrackKey: "v1", SourceIndex: 0, Color: col,
			Crop: &metadata.Crop{X: 8, Y: 4, W: 64, H: 36}, CodedW: 96, CodedH: 54, W: 64, H: 36}, 64, 36},
		{"crop-rot90", timeline.Layer{TrackKey: "v1", SourceIndex: 0, Rotation: 90, Color: col,
			Crop: &metadata.Crop{X: 8, Y: 4, W: 64, H: 36}, CodedW: 96, CodedH: 54, W: 36, H: 64}, 36, 64},
		{"scaled-rot0", timeline.Layer{TrackKey: "v0", SourceIndex: 1, Color: col,
			CodedW: 96, CodedH: 54, W: 24, H: 14}, 24, 14},
		{"scaled-rot90", timeline.Layer{TrackKey: "v0", SourceIndex: 1, Rotation: 90, Color: col,
			CodedW: 96, CodedH: 54, W: 14, H: 24}, 14, 24},
		{"scaled-crop", timeline.Layer{TrackKey: "v1", SourceIndex: 0, Color: col,
			Crop: &metadata.Crop{X: 8, Y: 4, W: 64, H: 36}, CodedW: 96, CodedH: 54, W: 16, H: 9}, 16, 9},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := NewEngine(42)
			f := timeline.Frame{Width: c.w, Height: c.h, Layers: []timeline.Layer{c.l}}
			fwd, err := e.RenderFrame(f)
			if err != nil {
				t.Fatal(err)
			}
			inv := e.InverseSample(f)
			if got := PSNR(fwd.Image, inv); got < 99 {
				t.Fatalf("geometry PSNR = %.2f dB (forward and inverse disagree)", got)
			}
			if got := SSIM(fwd.Image, inv); got < 0.999 {
				t.Fatalf("geometry SSIM = %.4f", got)
			}
		})
	}
}

// TestColorTransformQuantizationBounds verifies that the production 8-bit path
// is a faithful quantization of the exact floating-point path. This is the
// fidelity definition behind the PSNR/SSIM acceptance metrics.
func TestColorTransformQuantizationBounds(t *testing.T) {
	e := NewEngine(7)
	col := metadata.NewColor(metadata.PrimariesBT2020, metadata.TransferPQ, metadata.MatrixBT2020, true, metadata.SourceColr)
	l := timeline.Layer{TrackKey: "v0", SourceIndex: 0, CodedW: 64, CodedH: 36, W: 64, H: 36, Color: col}
	f := timeline.Frame{Width: 64, Height: 36, Layers: []timeline.Layer{l}}
	rounded, _ := e.RenderFrame(f)
	exact, _ := e.RenderReference(f)
	psnr := PSNR(rounded.Image, exact.Image)
	if psnr < 40 {
		t.Fatalf("quantization PSNR = %.2f dB, transform appears unstable", psnr)
	}
	if psnr >= 100 {
		t.Fatal("rounded and exact paths are identical; the gate tests nothing")
	}
	// A wrong color decision must be detectable: an identity transform differs
	// from the exact path by far more than quantization noise.
	plain := timeline.Frame{Width: 64, Height: 36, Layers: []timeline.Layer{{
		TrackKey: "v0", SourceIndex: 0, CodedW: 64, CodedH: 36, W: 64, H: 36,
		Color: metadata.DefaultColor}}}
	plainOut, _ := e.RenderFrame(plain)
	if got := PSNR(plainOut.Image, exact.Image); got >= 40 {
		t.Fatalf("identity color is not distinguishable (%.2f dB)", got)
	}
}
