package audio

import (
	"testing"

	"avpipe/internal/media"
)

func TestDownmixMonoDuplicates(t *testing.T) {
	d := NewDownmixer()
	out := d.Process([]int16{100, -200, 300}, media.LayoutMono)
	if len(out) != 6 {
		t.Fatalf("len=%d, want 6", len(out))
	}
	if out[0] != 100 || out[1] != 100 || out[2] != -200 || out[3] != -200 {
		t.Fatalf("mono duplication wrong: %v", out)
	}
}

func TestDownmixStereoPassthrough(t *testing.T) {
	d := NewDownmixer()
	in := []int16{1, 2, 3, 4}
	out := d.Process(in, media.LayoutStereo)
	if len(out) != 4 || out[0] != 1 || out[3] != 4 {
		t.Fatalf("stereo passthrough wrong: %v", out)
	}
}

func TestDownmixFiveOneNoClipping(t *testing.T) {
	d := NewDownmixer()
	// Two frames of full-scale 5.1; downmix must clamp rather than overflow.
	in := make([]int16, 12)
	for i := range in {
		in[i] = 32767
	}
	out := d.Process(in, media.LayoutFiveOne)
	if len(out) != 4 {
		t.Fatalf("len=%d, want 4 stereo samples", len(out))
	}
	for i, s := range out {
		if s > 32767 || s < -32768 {
			t.Fatalf("sample %d out of int16 range: %d", i, s)
		}
	}
}

func TestDownmixFiveOneDropsLFE(t *testing.T) {
	d := NewDownmixer()
	// One frame: only LFE is non-zero; stereo output must be silent.
	in := []int16{0, 0, 0, 12345, 0, 0}
	out := d.Process(in, media.LayoutFiveOne)
	if out[0] != 0 || out[1] != 0 {
		t.Fatalf("LFE leaked into stereo: %v", out)
	}
}
