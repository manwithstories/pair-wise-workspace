package mpd

import (
	"math"
	"testing"
)

func TestRescale(t *testing.T) {
	got, err := Rescale(192000, 48000, 90000)
	if err != nil || got != 360000 {
		t.Fatalf("Rescale = %d, %v; want 360000", got, err)
	}
	// rounding: 1 tick at 44100 -> 2.04 -> 2 at 90000
	got, _ = Rescale(1, 44100, 90000)
	if got != 2 {
		t.Fatalf("rounding = %d, want 2", got)
	}
	if _, err := Rescale(math.MaxInt64, 1, 90000); err != ErrOverflow {
		t.Fatalf("want overflow, got %v", err)
	}
	if _, err := Rescale(-1, 1, 1); err == nil {
		t.Fatal("want error for negative value")
	}
}

func TestAccumulateOverflow(t *testing.T) {
	if _, err := Accumulate(math.MaxInt64-1, []int64{5}); err != ErrOverflow {
		t.Fatalf("want overflow, got %v", err)
	}
	b, err := Accumulate(10, []int64{5, 5})
	if err != nil || len(b) != 3 || b[2] != 20 {
		t.Fatalf("Accumulate = %v, %v", b, err)
	}
}

func TestBuildAndExpandTimeline(t *testing.T) {
	bounds := []int64{100, 160, 220, 280, 300}
	tl, err := BuildTimeline(bounds)
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.S) != 2 || tl.S[0].D != 60 || *tl.S[0].R != 2 || tl.S[1].D != 20 {
		t.Fatalf("unexpected runs: %+v", tl.S)
	}
	pts, err := ExpandTimeline(tl, 101)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 4 || pts[0].Number != 101 || pts[3].Number != 104 {
		t.Fatalf("numbering: %+v", pts)
	}
	if pts[0].T != 100 || pts[3].T != 280 || pts[3].D != 20 {
		t.Fatalf("times: %+v", pts)
	}
}

func TestExpandTimelineErrors(t *testing.T) {
	if _, err := ExpandTimeline(&SegmentTimeline{S: []S{{D: 10}}}, 1); err == nil {
		t.Fatal("want error for missing t")
	}
	neg := int64(-1)
	if _, err := ExpandTimeline(&SegmentTimeline{S: []S{{T: &neg, D: 10, R: &neg}}}, 1); err == nil {
		t.Fatal("want error for negative r")
	}
}
