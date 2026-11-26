package media

import (
	"math"
	"testing"
)

const wrapMod = int64(1) << 33 // 2^33

func TestUnwrapPTSWrap(t *testing.T) {
	raw := []int64{wrapMod - 3600, wrapMod - 1800, 0, 1800, 3600}
	got, err := UnwrapPTS(raw, wrapMod)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(got); i++ {
		if got[i]-got[i-1] != 1800 {
			t.Fatalf("unwrapped[%d]=%d, want uniform 1800 steps: %v", i, got[i], got)
		}
	}
}

func TestUnwrapPTSAmbiguous(t *testing.T) {
	raw := []int64{0, wrapMod / 2}
	if _, err := UnwrapPTS(raw, wrapMod); err == nil {
		t.Fatal("half-period jump accepted")
	}
}

func TestUnwrapPTSBackward(t *testing.T) {
	if _, err := UnwrapPTS([]int64{5000, 1000}, wrapMod); err == nil {
		t.Fatal("silent backward pts accepted")
	}
	if _, err := UnwrapPTS([]int64{5000, 1000}, 0); err == nil {
		t.Fatal("backward pts accepted with modulus 0")
	}
}

func tlFrames(seqs []int64, pts []int64, dur int64) []FrameInput {
	fs := make([]FrameInput, len(seqs))
	for i := range seqs {
		fs[i] = FrameInput{Track: "camA", Seq: seqs[i], PTS: pts[i], Dur: dur}
	}
	return fs
}

func TestBuildTimelineVFR(t *testing.T) {
	tb := Timebase{1, 90000}
	// VFR：时长缺失，由相邻有效时间推导
	fs := tlFrames([]int64{0, 1, 2}, []int64{0, 3000, 9000}, 0)
	slots, err := BuildTrackTimeline(fs, tb, nil, 0, 40)
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]float64{{0, 100.0 / 3}, {100.0 / 3, 200.0 / 3}, {100, 40}}
	for i, s := range slots {
		if math.Abs(s.StartMs-want[i][0]) > 1e-9 || math.Abs(s.DurMs-want[i][1]) > 1e-9 {
			t.Fatalf("slot %d = %+v, want start %v dur %v", i, s, want[i][0], want[i][1])
		}
	}
}

func TestBuildTimelineGap(t *testing.T) {
	tb := Timebase{1, 90000}
	// 缺 seq 1、2：写入 GAP，且 seq 3 的时间不被挪动
	fs := tlFrames([]int64{0, 3}, []int64{0, 10800}, 3600)
	slots, err := BuildTrackTimeline(fs, tb, nil, 0, 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 4 {
		t.Fatalf("slots = %d, want 4", len(slots))
	}
	if !slots[1].Gap || !slots[2].Gap || slots[0].Gap || slots[3].Gap {
		t.Fatalf("gap flags wrong: %+v", slots)
	}
	if slots[3].StartMs != 120 {
		t.Fatalf("seq 3 start = %v, want 120 (must not shift)", slots[3].StartMs)
	}
	if slots[1].StartMs != 40 || slots[2].StartMs != 80 {
		t.Fatalf("gap slots at %v, %v, want 40, 80", slots[1].StartMs, slots[2].StartMs)
	}
}

func TestBuildTimelineWrapAndDrift(t *testing.T) {
	tb := Timebase{1, 90000}
	base := wrapMod - 3600
	fs := tlFrames([]int64{0, 1, 2}, []int64{base, 0, 3600}, 3600)
	dc, err := FitDrift(tb, []Anchor{{PTS: base, RefMs: tb.ToMillis(base) + 25}}, 1e-4)
	if err != nil {
		t.Fatal(err)
	}
	slots, err := BuildTrackTimeline(fs, tb, dc, wrapMod, 40)
	if err != nil {
		t.Fatal(err)
	}
	start0 := tb.ToMillis(base) + 25
	for i, s := range slots {
		want := start0 + float64(i)*40
		if math.Abs(s.StartMs-want) > 1e-6 {
			t.Fatalf("slot %d start = %v, want %v", i, s.StartMs, want)
		}
	}
}

func TestBuildTimelineErrors(t *testing.T) {
	tb := Timebase{1, 90000}
	dup := tlFrames([]int64{0, 0}, []int64{0, 3600}, 3600)
	if _, err := BuildTrackTimeline(dup, tb, nil, 0, 40); err == nil {
		t.Fatal("duplicate seq accepted")
	}
	back := tlFrames([]int64{0, 1}, []int64{7200, 3600}, 3600)
	if _, err := BuildTrackTimeline(back, tb, nil, 0, 40); err == nil {
		t.Fatal("backward pts accepted")
	}
}
