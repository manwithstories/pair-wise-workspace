package source

import (
	"bytes"
	"testing"
)

func TestGenerateReproducible(t *testing.T) {
	cfg := Config{Seed: 7, Frames: 16, Width: 64, Height: 64}
	var b1, b2 bytes.Buffer
	h1, err := Generate(&b1, cfg)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := Generate(&b2, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 || !bytes.Equal(b1.Bytes(), b2.Bytes()) {
		t.Fatal("same seed must produce identical output")
	}
	cfg.Seed = 8
	var b3 bytes.Buffer
	h3, _ := Generate(&b3, cfg)
	if h3 == h1 {
		t.Fatal("different seed should change output")
	}
	if b1.Len() != cfg.Frames*FrameSize(cfg.Width, cfg.Height) {
		t.Fatalf("size: got %d", b1.Len())
	}
}

func TestLayoutHasCutAndTwoNoiseSegments(t *testing.T) {
	cut, na, nb := Layout(240)
	if cut != 120 {
		t.Fatalf("cut at %d, want 120", cut)
	}
	if na[1] <= na[0] || nb[1] <= nb[0] {
		t.Fatalf("empty noise ranges: %v %v", na, nb)
	}
	if na[0] >= cut || nb[0] <= cut {
		t.Fatalf("noise segments should straddle the cut: %v %v cut=%d", na, nb, cut)
	}
	if na[0] >= nb[0] {
		t.Fatalf("noise ranges overlap or unordered: %v %v", na, nb)
	}
}

func TestValidate(t *testing.T) {
	if err := (Config{Seed: 1, Frames: 4, Width: 64, Height: 64}).Validate(); err == nil {
		t.Fatal("too few frames should fail")
	}
	if err := (Config{Seed: 1, Frames: 16, Width: 63, Height: 64}).Validate(); err == nil {
		t.Fatal("odd width should fail")
	}
	if err := (Config{Seed: 1, Frames: 16, Width: 64, Height: 64}).Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestReaderRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/s.yuv"
	cfg := Config{Seed: 3, Frames: 8, Width: 32, Height: 32}
	if _, err := GenerateFile(path, cfg); err != nil {
		t.Fatal(err)
	}
	r, err := OpenReader(path, 32, 32)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Frames() != 8 {
		t.Fatalf("frames: got %d", r.Frames())
	}
	got := make([]byte, r.FrameSize())
	want := make([]byte, r.FrameSize())
	for i := 0; i < 8; i++ {
		if err := r.ReadFrame(i, got); err != nil {
			t.Fatal(err)
		}
		cfg.RenderFrame(want, i)
		if !bytes.Equal(got, want) {
			t.Fatalf("frame %d mismatch", i)
		}
	}
	if err := r.ReadFrame(8, got); err == nil {
		t.Fatal("out-of-range read should error")
	}
}
