package audio

import (
	"math"
	"testing"

	"mediapipeline/internal/mediaerr"
)

func TestLayoutChannelsAndUnknown(t *testing.T) {
	cases := map[Layout]int{LayoutMono: 1, LayoutStereo: 2, Layout51: 6}
	for l, want := range cases {
		got, merr := l.Channels()
		if merr != nil || got != want {
			t.Fatalf("%s channels=%d err=%v want %d", l, got, merr, want)
		}
	}
	if _, merr := Layout("atmos").Channels(); merr == nil || merr.Kind != mediaerr.KindLayout {
		t.Fatal("unknown layout must produce a locatable error")
	}
}

func TestMonoStereoConversions(t *testing.T) {
	out, merr := Normalize(LayoutMono, LayoutStereo, []float64{0.5, -0.25})
	if merr != nil {
		t.Fatal(merr)
	}
	if len(out) != 4 || out[0] != 0.5 || out[1] != 0.5 || out[2] != -0.25 || out[3] != -0.25 {
		t.Fatalf("mono->stereo wrong: %v", out)
	}
	back, merr := Normalize(LayoutStereo, LayoutMono, []float64{0.4, 0.8})
	if merr != nil || len(back) != 1 || math.Abs(back[0]-0.6) > 1e-12 {
		t.Fatalf("stereo->mono wrong: %v err=%v", back, merr)
	}
}

func TestStereoTo51FillRules(t *testing.T) {
	out, merr := Normalize(LayoutStereo, Layout51, []float64{0.6, 0.2})
	if merr != nil {
		t.Fatal(merr)
	}
	want := []float64{0.6, 0.2, 0.4, 0, 0, 0} // FL, FR, C, LFE, BL, BR
	if len(out) != len(want) {
		t.Fatalf("5.1 frame size wrong: %v", out)
	}
	for i := range want {
		if math.Abs(out[i]-want[i]) > 1e-12 {
			t.Fatalf("stereo->5.1 wrong: %v want %v", out, want)
		}
	}
}

func TestMonoTo51(t *testing.T) {
	out, merr := Normalize(LayoutMono, Layout51, []float64{0.9})
	if merr != nil {
		t.Fatal(merr)
	}
	want := []float64{0.9, 0.9, 0.9, 0, 0, 0}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("mono->5.1 wrong: %v want %v", out, want)
		}
	}
}

func Test51DownMixExcludesLFE(t *testing.T) {
	frame := []float64{1, 1, 1, 100, 1, 1} // LFE spike must not leak
	st, merr := Normalize(Layout51, LayoutStereo, frame)
	if merr != nil {
		t.Fatal(merr)
	}
	if math.Abs(st[0]-st[1]) > 1e-9 {
		t.Fatalf("symmetric frame must fold symmetrically: %v", st)
	}
	// 1 + 0.7071 + 0.7071 ≈ 2.4142
	if math.Abs(st[0]-2.4142) > 1e-3 {
		t.Fatalf("5.1->stereo coefficient wrong: %v", st)
	}
	mono, merr := Normalize(Layout51, LayoutMono, frame)
	if merr != nil || math.Abs(mono[0]) > 2 {
		t.Fatalf("5.1->mono must exclude LFE and normalize: %v err=%v", mono, merr)
	}
}

func TestIdentityAndIncompleteFrame(t *testing.T) {
	in := []float64{0.1, 0.2, 0.3, 0.4}
	out, merr := Normalize(LayoutStereo, LayoutStereo, in)
	if merr != nil || len(out) != 4 || &out[0] == &in[0] {
		t.Fatal("identity conversion must copy, not alias")
	}
	if _, merr := Normalize(LayoutStereo, LayoutMono, []float64{0.1, 0.2, 0.3}); merr == nil ||
		merr.Kind != mediaerr.KindIncompleteFrame {
		t.Fatalf("incomplete frame must error, got %v", merr)
	}
}

func TestEmptyInput(t *testing.T) {
	out, merr := Normalize(LayoutMono, Layout51, nil)
	if merr != nil || len(out) != 0 {
		t.Fatalf("empty input must yield empty output: %v err=%v", out, merr)
	}
}
