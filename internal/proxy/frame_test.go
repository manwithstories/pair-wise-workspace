package proxy

import (
	"bytes"
	"image/color"
	"testing"
)

func TestGenerateIsDeterministic(t *testing.T) {
	a := Generate(73, 3600, 32)
	b := Generate(73, 3600, 32)
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			if a.NRGBAAt(x, y) != b.NRGBAAt(x, y) {
				t.Fatalf("pixel (%d,%d) differs between identical calls", x, y)
			}
		}
	}
	c := Generate(73, 7200, 32)
	if PixelHash(a) == PixelHash(c) {
		t.Fatalf("different ticks produced the same pixels")
	}
}

func TestNormaliseProducesSquareGrayscale(t *testing.T) {
	src := Generate(1, 0, 100)
	got := Normalise(src, 48)
	if got.Bounds().Dx() != 48 || got.Bounds().Dy() != 48 {
		t.Fatalf("bounds = %v, want 48x48", got.Bounds())
	}
	// Grayscale output has equal channels.
	r, g, b, _ := got.At(10, 10).RGBA()
	if r != g || g != b {
		t.Fatalf("pixel is not gray: %d %d %d", r, g, b)
	}
	nc, ok := got.At(10, 10).(color.Gray)
	if ok {
		if nc.Y != byte(r>>8) {
			t.Fatalf("gray value mismatch")
		}
	}
}

func TestPixelHashStableAndSensitive(t *testing.T) {
	a := Make(5, 1000, 32)
	b := Make(5, 1000, 32)
	if a.Hash != b.Hash {
		t.Fatalf("same input hashed differently")
	}
	if len(a.Hash) != 64 {
		t.Fatalf("hash length = %d, want 64", len(a.Hash))
	}
	c := Make(6, 1000, 32)
	if a.Hash == c.Hash {
		t.Fatalf("different seeds hashed identically")
	}
}

func TestMakeRecordsGeometry(t *testing.T) {
	f := Make(1, 90, 40)
	if f.Width != 40 || f.Height != 40 || f.Ticks != 90 {
		t.Fatalf("frame = %+v", f)
	}
	if f.Img == nil {
		t.Fatalf("nil image")
	}
}

func TestEncodePNGRoundTrips(t *testing.T) {
	f := Make(2, 180, 16)
	var buf bytes.Buffer
	if err := EncodePNG(&buf, f); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte{0x89, 'P', 'N', 'G'}) {
		t.Fatalf("output is not a PNG")
	}
}

func TestBuildCombinedHash(t *testing.T) {
	s := Build(9, []uint64{0, 3600, 7200}, 16)
	if len(s.Frames) != 3 || len(s.Hashes) != 3 {
		t.Fatalf("set = %+v", s)
	}
	if len(s.CombinedHash()) != 64 {
		t.Fatalf("combined hash is not 64 hex chars")
	}
	s2 := Build(9, []uint64{0, 3600, 7200}, 16)
	if s.CombinedHash() != s2.CombinedHash() {
		t.Fatalf("combined hash not reproducible")
	}
	s3 := Build(9, []uint64{0, 3600, 10800}, 16)
	if s.CombinedHash() == s3.CombinedHash() {
		t.Fatalf("different frame sets share a combined hash")
	}
}

func TestDefaultSizeOnNonPositive(t *testing.T) {
	f := Make(1, 0, 0)
	if f.Width != DefaultSize || f.Height != DefaultSize {
		t.Fatalf("default size not applied: %+v", f)
	}
}
