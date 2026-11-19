package audio

import (
	"testing"

	"avpipe/internal/fault"
)

func TestMapLayoutFillsMissingChannelsWithSilence(t *testing.T) {
	src, _ := ParseLayout("stereo")
	dst, _ := ParseLayout("5.1")
	in := []float64{0.5, -0.5, 0.25, -0.25}
	out, plan, err := MapLayout(src, in, 2, dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2*6 {
		t.Fatalf("len=%d", len(out))
	}
	// FL=0.5, FR=-0.5, FC/LFE/BL/BR are explicit silence.
	want := []float64{0.5, -0.5, 0, 0, 0, 0, 0.25, -0.25, 0, 0, 0, 0}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("sample %d=%v want %v", i, out[i], want[i])
		}
	}
	if len(plan.Mapped) != 2 {
		t.Fatalf("mapped=%v", plan.Mapped)
	}
	if len(plan.SilenceFills) != 4 {
		t.Fatalf("fills=%v", plan.SilenceFills)
	}
}

func TestMapLayoutDropsExtraChannels(t *testing.T) {
	src, _ := ParseLayout("5.1")
	dst, _ := ParseLayout("mono")
	in := make([]float64, 6)
	in[2] = 0.7 // FC is the mono target
	out, _, err := MapLayout(src, in, 1, dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0] != 0.7 {
		t.Fatalf("out=%v", out)
	}
}

func TestMapLayoutRejectsSizeMismatch(t *testing.T) {
	src, _ := ParseLayout("stereo")
	dst, _ := ParseLayout("stereo")
	_, _, err := MapLayout(src, []float64{1}, 1, dst)
	e := fault.Root(err)
	if e == nil || e.Code != fault.CodeInvalidInput {
		t.Fatalf("want invalid input, got %v", err)
	}
}

func TestLayoutForChannels(t *testing.T) {
	for n, want := range map[int]string{1: "mono", 2: "stereo", 4: "quad", 6: "5.1", 8: "7.1"} {
		l, err := LayoutForChannels(n)
		if err != nil {
			t.Fatal(err)
		}
		if l.Name != want {
			t.Fatalf("%dch -> %s want %s", n, l.Name, want)
		}
	}
	if _, err := LayoutForChannels(3); err == nil {
		t.Fatal("3 channels accepted")
	}
}

func TestMaskToLayout(t *testing.T) {
	l, ok := MaskToLayout(0x3, 2)
	if !ok || l.Name != "stereo" {
		t.Fatalf("stereo mask -> %v %v", l, ok)
	}
	if _, ok := MaskToLayout(0x1|0x4, 2); ok {
		t.Fatal("nonsense mask accepted")
	}
}
