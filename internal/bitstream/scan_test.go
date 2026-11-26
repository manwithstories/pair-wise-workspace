package bitstream

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func samplePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sampleJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCheckPNG(t *testing.T) {
	data := samplePNG(t)
	if err := CheckPNG(data); err != nil {
		t.Fatalf("valid PNG rejected: %v", err)
	}
	if err := CheckPNG(data[:len(data)/2]); !errors.Is(err, ErrTruncated) {
		t.Fatalf("truncated: %v", err)
	}
	bad := bytes.Clone(data)
	bad[0] = 0x00
	if err := CheckPNG(bad); !errors.Is(err, ErrStructure) {
		t.Fatalf("bad signature: %v", err)
	}
	bad = bytes.Clone(data)
	bad[16] ^= 0xFF // corrupt IHDR payload -> CRC mismatch
	if err := CheckPNG(bad); !errors.Is(err, ErrStructure) {
		t.Fatalf("bad CRC: %v", err)
	}
	if err := CheckPNG(append(bytes.Clone(data), 0x01)); !errors.Is(err, ErrStructure) {
		t.Fatal("expected trailing-bytes error")
	}
}

func TestCheckJPEG(t *testing.T) {
	data := sampleJPEG(t)
	if err := CheckJPEG(data); err != nil {
		t.Fatalf("valid JPEG rejected: %v", err)
	}
	if err := CheckJPEG(data[:len(data)-2]); !errors.Is(err, ErrTruncated) {
		t.Fatalf("truncated: %v", err)
	}
	bad := bytes.Clone(data)
	bad[1] = 0x00
	if err := CheckJPEG(bad); !errors.Is(err, ErrStructure) {
		t.Fatalf("bad SOI: %v", err)
	}
}

func TestKeyframeViolations(t *testing.T) {
	key := []bool{true, false, false, true, false, false, false, true}
	if v := KeyframeViolations(key, 4); len(v) != 0 {
		t.Fatalf("unexpected violations: %v", v)
	}
	key = []bool{true, false, false, true, false, false, false, false, true}
	if v := KeyframeViolations(key, 3); len(v) != 1 || v[0] != 8 {
		t.Fatalf("want violation at 8, got %v", v)
	}
	if v := KeyframeViolations([]bool{false, true}, 4); len(v) != 1 || v[0] != 0 {
		t.Fatalf("want head violation, got %v", v)
	}
	if v := KeyframeViolations(nil, 4); v != nil {
		t.Fatalf("empty: %v", v)
	}
}
