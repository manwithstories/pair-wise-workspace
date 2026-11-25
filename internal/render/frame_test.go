package render

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/fixture"
	"relay.local/relay/internal/model"
)

func jpegSample(t *testing.T, i, w, h int) []byte {
	t.Helper()
	b, err := fixture.JPEGFrame(i, w, h)
	if err != nil {
		t.Fatalf("fixture frame: %v", err)
	}
	return b
}

func TestRotateThenCrop(t *testing.T) {
	// A 8x4 source rotated 90 becomes 4x8; a 1-pixel inset on each side
	// leaves 2x6.
	src := image.NewRGBA(image.Rect(0, 0, 8, 4))
	desc := model.Description{Rotation: 90, Crop: model.Rect{Left: 1, Top: 1, Right: 1, Bottom: 1}}
	got, err := Frame(mustJPEG(t, src), desc, Options{})
	if err != nil {
		t.Fatalf("frame: %v", err)
	}
	if b := got.Bounds(); b.Dx() != 2 || b.Dy() != 6 {
		t.Fatalf("bounds = %dx%d, want 2x6", b.Dx(), b.Dy())
	}
}

func TestRotationOrientations(t *testing.T) {
	// Encode a distinctive single-pixel image at each corner and verify the
	// pixels move as clockwise rotation requires.
	src := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			src.SetRGBA(x, y, toColor(0, 0, 255)) // blue field
		}
	}
	// A solid red 4x4 block in the top-left quarter.
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			src.SetRGBA(x, y, toColor(255, 0, 0))
		}
	}
	jpg := mustJPEG(t, src)
	cases := []struct {
		deg  int
		w, h int
		// where the red pixel should land
		x, y int
	}{
		{0, 8, 8, 2, 2},
		{90, 8, 8, 5, 2},  // top-left block goes top-right
		{180, 8, 8, 5, 5}, // bottom-right
		{270, 8, 8, 2, 5}, // bottom-left
	}
	for _, c := range cases {
		img, err := Frame(jpg, model.Description{Rotation: c.deg}, Options{})
		if err != nil {
			t.Fatalf("rotation %d: %v", c.deg, err)
		}
		if b := img.Bounds(); b.Dx() != c.w || b.Dy() != c.h {
			t.Fatalf("rotation %d bounds = %dx%d, want %dx%d", c.deg, b.Dx(), b.Dy(), c.w, c.h)
		}
		_ = c.w
		// JPEG is lossy, so compare loosely against a warm pixel.
		r, _, _, _ := img.At(c.x, c.y).RGBA()
		if r < 0x8000 {
			t.Errorf("rotation %d: pixel at (%d,%d) not warm", c.deg, c.x, c.y)
		}
		// The opposite corner must stay cold, proving orientation rather than
		// mere presence of red.
		ox, oy := c.w-1-c.x, c.h-1-c.y
		r2, _, _, _ := img.At(ox, oy).RGBA()
		if r2 > 0x8000 {
			t.Errorf("rotation %d: opposite pixel at (%d,%d) is warm", c.deg, ox, oy)
		}
	}
}

func TestUnsupportedCodecKeepsHash(t *testing.T) {
	dir := t.TempDir()
	sample := []byte{0x00, 0x00, 0x00, 0x01, 0x67}
	res := WriteFrame(dir, 3, sample, model.CodecH264, model.Description{}, Options{})
	if res.Written {
		t.Fatal("non-decodable codec produced a file")
	}
	if len(res.Sample) != 64 {
		t.Fatalf("sample hash = %q, want 64 hex chars", res.Sample)
	}
	fe, ok := fault.As(res.Err)
	if !ok || fe.Kind != fault.KindUnsupported {
		t.Fatalf("expected unsupported fault, got %v", res.Err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("unexpected files written: %v", entries)
	}
}

func TestWriteFrameIsDeterministic(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	sample := jpegSample(t, 5, 16, 16)
	desc := model.Description{Rotation: 90}
	ra := WriteFrame(a, 5, sample, model.CodecMJPEG, desc, Options{})
	rb := WriteFrame(b, 5, sample, model.CodecMJPEG, desc, Options{})
	if ra.Err != nil || rb.Err != nil {
		t.Fatalf("errors: %v / %v", ra.Err, rb.Err)
	}
	if ra.OutHash != rb.OutHash {
		t.Fatalf("PNG digests differ: %s vs %s", ra.OutHash, rb.OutHash)
	}
	da, _ := os.ReadFile(filepath.Join(a, ra.Path))
	db, _ := os.ReadFile(filepath.Join(b, rb.Path))
	if !bytes.Equal(da, db) {
		t.Fatal("PNG bytes differ between identical runs")
	}
	if _, err := png.Decode(bytes.NewReader(da)); err != nil {
		t.Fatalf("output is not a PNG: %v", err)
	}
}

func TestRejectsDecompressionBombDimensions(t *testing.T) {
	sample := jpegSample(t, 0, 64, 64)
	_, err := Frame(sample, model.Description{}, Options{MaxPixels: 100})
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindLimit {
		t.Fatalf("expected pixel limit fault, got %v", err)
	}
}

func TestCorruptJPEGIsRecoverableFault(t *testing.T) {
	_, err := Frame([]byte("not a jpeg"), model.Description{}, Options{})
	fe, ok := fault.As(err)
	if !ok || !fe.Recoverable {
		t.Fatalf("expected recoverable fault, got %v", err)
	}
}

func toColor(r, g, b uint8) color.RGBA { return color.RGBA{R: r, G: g, B: b, A: 255} }

func mustJPEG(t *testing.T, img *image.RGBA) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}
