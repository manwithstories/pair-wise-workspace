package normalize

import (
	"testing"

	"mediad/internal/media"
)

func TestDownmixMonoStereo(t *testing.T) {
	out, f := Downmix([]int16{100, -200}, 1, LayoutMono)
	if f != nil {
		t.Fatal(f)
	}
	if len(out) != 4 || out[0] != 100 || out[1] != 100 || out[2] != -200 {
		t.Fatalf("mono downmix: %v", out)
	}
	st := []int16{1, 2, 3, 4}
	out, f = Downmix(st, 2, LayoutStereo)
	if f != nil || len(out) != 4 || out[3] != 4 {
		t.Fatalf("stereo passthrough: %v %v", out, f)
	}
}

func TestDownmix51(t *testing.T) {
	// L=1000 R=2000 C=3000 LFE=9000 Ls=4000 Rs=5000
	src := []int16{1000, 2000, 3000, 9000, 4000, 5000}
	out, f := Downmix(src, 6, Layout51)
	if f != nil {
		t.Fatal(f)
	}
	wantL := clamp16(1000 + 0.7071*3000 + 0.7071*4000)
	wantR := clamp16(2000 + 0.7071*3000 + 0.7071*5000)
	if out[0] != wantL || out[1] != wantR {
		t.Fatalf("5.1 downmix: got %v want %v,%v", out, wantL, wantR)
	}
}

func TestMissingLayoutIsError(t *testing.T) {
	_, f := Downmix([]int16{1, 2, 3, 4, 5, 6, 7, 8}, 4, "")
	if f == nil || f.Kind != media.FaultLayoutMissing {
		t.Fatalf("want layout-missing fault, got %v", f)
	}
}

func TestResample441To48(t *testing.T) {
	r := NewResampler(44100, 48000)
	// 20 ms chunks of 882 source frames must yield exactly 960 out.
	src := make([]int16, 882*2)
	for i := range src {
		src[i] = int16(i % 1000)
	}
	total := 0
	for chunk := 0; chunk < 10; chunk++ {
		out := r.Process(src)
		if len(out) != 960*2 {
			t.Fatalf("chunk %d: got %d frames want 960", chunk, len(out)/2)
		}
		total += len(out) / 2
	}
	if total != 9600 {
		t.Fatalf("total=%d", total)
	}
}

func TestResampleIdentity48k(t *testing.T) {
	r := NewResampler(48000, 48000)
	src := []int16{10, -10, 20, -20, 30, -30, 40, -40}
	out := r.Process(src)
	if len(out) != len(src) {
		t.Fatalf("identity resample changed length: %d", len(out))
	}
	for i := range src {
		if out[i] != src[i] {
			t.Fatalf("identity resample changed sample %d: %d != %d", i, out[i], src[i])
		}
	}
}

func TestResamplerStateRoundTrip(t *testing.T) {
	r := NewResampler(44100, 48000)
	src := make([]int16, 882*2)
	for i := range src {
		src[i] = int16(i * 3)
	}
	a := r.Process(src)
	r2 := NewResampler(44100, 48000)
	r2.SetState(r.State())
	b1 := r.Process(src)
	b2 := r2.Process(src)
	if len(b1) != len(b2) {
		t.Fatalf("state restore changed output length")
	}
	for i := range b1 {
		if b1[i] != b2[i] {
			t.Fatalf("state restore diverged at sample %d", i)
		}
	}
	_ = a
}

func TestColorConflictKeptAndDiagnosed(t *testing.T) {
	reg := NewColorRegistry()
	reg.Observe(9, ColorInfo{Primaries: "bt709", Transfer: "bt709", Matrix: "bt709", Range: "limited"})
	f := reg.Observe(9, ColorInfo{Primaries: "bt2020", Transfer: "bt709", Matrix: "bt709", Range: "limited"})
	if f == nil || f.Kind != media.FaultColorConflict {
		t.Fatalf("want color-conflict fault, got %v", f)
	}
	c := reg.Confirmed(9)
	if c.Primaries != "bt709" {
		t.Fatalf("confirmed value overwritten: %+v", c)
	}
	if len(reg.Diagnostics()) == 0 {
		t.Fatal("conflict not recorded in diagnostics")
	}
}

func TestColorMissingNotDefaulted(t *testing.T) {
	reg := NewColorRegistry()
	reg.Observe(9, ColorInfo{Primaries: "bt709"}) // other fields missing
	c := reg.Confirmed(9)
	if c.Transfer != "" || c.Matrix != "" || c.Range != "" {
		t.Fatalf("missing fields filled with defaults: %+v", c)
	}
	if len(reg.Diagnostics()) != 3 {
		t.Fatalf("diagnostics=%v", reg.Diagnostics())
	}
	// A later update may fill a missing field without conflict.
	if f := reg.Observe(9, ColorInfo{Primaries: "bt709", Range: "full"}); f != nil {
		t.Fatal(f)
	}
	if reg.Confirmed(9).Range != "full" {
		t.Fatalf("missing field not filled: %+v", reg.Confirmed(9))
	}
}
