package clock

import "testing"

func TestRescale(t *testing.T) {
	if got := Rescale(1024, 48000, 90000); got != 1920 {
		t.Fatalf("1024@48k -> %d@90k", got)
	}
	if got := Rescale(90000, 90000, 90000); got != 90000 {
		t.Fatalf("identity: %d", got)
	}
	if got := Rescale(1, 3, 1); got != 0 { // 0.33 rounds to 0
		t.Fatalf("rounding: %d", got)
	}
}

func TestAlignContinuous(t *testing.T) {
	tl := NewTrackTimeline(1, 90000)
	if ms, k := tl.Align(0, 90000); ms != 0 || k != JumpNone {
		t.Fatalf("first: ms=%d k=%v", ms, k)
	}
	if ms, k := tl.Align(90000, 90000); ms != 90000 || k != JumpNone {
		t.Fatalf("second: ms=%d k=%v", ms, k)
	}
	if tl.Regressions != 0 || len(tl.Uncertain) != 0 {
		t.Fatalf("unexpected flags: %+v", tl)
	}
}

func TestAlignRegressionMarksUncertain(t *testing.T) {
	tl := NewTrackTimeline(1, 90000)
	tl.Align(0, 90000)
	ms, k := tl.Align(45000, 90000) // rewinds into the previous range
	if k != JumpRegression || ms != 45000 {
		t.Fatalf("ms=%d k=%v", ms, k)
	}
	if tl.Regressions != 1 || len(tl.Uncertain) != 1 {
		t.Fatalf("reg=%d unc=%v", tl.Regressions, tl.Uncertain)
	}
	u := tl.Uncertain[0]
	if u.Start != 45000 || u.End != 90000 || u.Reason == "" {
		t.Fatalf("uncertain=%+v", u)
	}
	// Timeline is not silently corrected: next expected stays continuous.
	if ms, k := tl.Align(90000, 90000); k == JumpNone && ms != 90000 {
		t.Fatalf("after regression: ms=%d k=%v", ms, k)
	}
}

func TestAlignForwardGap(t *testing.T) {
	tl := NewTrackTimeline(1, 90000)
	tl.Align(0, 90000)
	if _, k := tl.Align(180000, 90000); k != JumpForward {
		t.Fatalf("k=%v", k)
	}
	if tl.ForwardGaps != 1 {
		t.Fatalf("gaps=%d", tl.ForwardGaps)
	}
}

func TestTimelineSharedClockAcrossTracks(t *testing.T) {
	tl := NewTimeline()
	v := tl.Track(1, 90000)
	a := tl.Track(2, 48000)
	vms, _ := v.Align(90000, 90000)
	ams, _ := a.Align(48000, 48000)
	if vms != ams {
		t.Fatalf("tracks not aligned: v=%d a=%d", vms, ams)
	}
}
