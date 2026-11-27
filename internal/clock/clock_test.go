package clock

import "testing"

func TestTimecodeRoundTrip(t *testing.T) {
	cases := []struct {
		rate FrameRate
		tc   Timecode
	}{
		{Rate24, Timecode{Hour: 1, Min: 2, Sec: 3, Frame: 10, Rate: Rate24}},
		{Rate25, Timecode{Hour: 0, Min: 0, Sec: 0, Frame: 24, Rate: Rate25}},
		{Rate30, Timecode{Hour: 2, Min: 59, Sec: 59, Frame: 29, Rate: Rate30}},
		{Rate2997, Timecode{Hour: 1, Min: 0, Sec: 0, Frame: 0, Rate: Rate2997}},
		{Rate2997DF, Timecode{Hour: 0, Min: 10, Sec: 0, Frame: 0, Rate: Rate2997DF}},
		{Rate2997DF, Timecode{Hour: 1, Min: 23, Sec: 45, Frame: 17, Rate: Rate2997DF}},
	}
	for _, c := range cases {
		ns, err := TimecodeToNanos(c.rate, c.tc.Hour, c.tc.Min, c.tc.Sec, c.tc.Frame)
		if err != nil {
			t.Fatalf("TimecodeToNanos(%v): %v", c.tc, err)
		}
		back := NanosToTimecode(c.rate, ns)
		if back != c.tc {
			t.Errorf("round trip %s: got %s want %s (ns=%d)",
				c.rate.Nominal, back, c.tc, ns)
		}
	}
}

func TestDropFrameRejectsIllegalFrames(t *testing.T) {
	// Frames 0 and 1 of each non-ten-minute minute are dropped.
	for _, m := range []int{1, 5, 9, 11, 59} {
		for _, fr := range []int{0, 1} {
			if ValidTimecode(Rate2997DF, 0, m, 0, fr) {
				t.Errorf("frame %d at minute %d should be illegal (dropped)", fr, m)
			}
			if _, err := TimecodeToNanos(Rate2997DF, 0, m, 0, fr); err == nil ||
				!HasCode(err, CodeIllegalFrame) {
				t.Errorf("want CodeIllegalFrame for 00:%02d:00;%02d, got %v", m, fr, err)
			}
		}
	}
	// Ten-minute boundaries keep frames 0..1.
	if !ValidTimecode(Rate2997DF, 0, 10, 0, 0) {
		t.Error("00:10:00;00 must be legal")
	}
	// Frame 30 never exists at 29.97.
	if ValidTimecode(Rate2997DF, 0, 0, 1, 30) {
		t.Error("frame 30 must be out of range")
	}
	// NDF standard must not accept the drop separator.
	if _, err := ParseTimecode(Rate30, "00:01:00;05"); err == nil {
		t.Error("drop separator on NDF rate must fail")
	}
}

func TestDropFrameTenMinuteContinuity(t *testing.T) {
	// 10 minutes at 29.97 DF must contain exactly 17982 frames and map to
	// 00:10:00;00.
	ns, err := TimecodeToNanos(Rate2997DF, 0, 10, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantFrames := Rate2997DF.FramesPer10Min()
	if wantFrames != 17982 {
		t.Fatalf("FramesPer10Min=%d want 17982", wantFrames)
	}
	frames := Rat{Num: Rate2997DF.Num, Den: Rate2997DF.Den * Second}.FloorDiv(ns)
	if frames != wantFrames {
		t.Errorf("frames at 10 min = %d want %d", frames, wantFrames)
	}
}

func TestRepairTimeline(t *testing.T) {
	pts := []Point{
		{Track: "v", Seq: 0, TS: 10 * Millisecond},
		{Track: "v", Seq: 1, TS: 10 * Millisecond}, // duplicate -> nudge +1ns
		{Track: "v", Seq: 2, TS: 30 * Millisecond},
		{Track: "v", Seq: 3, TS: 20 * Millisecond}, // rollback 10ms -> shift
		{Track: "v", Seq: 4, TS: 600 * Millisecond},
		{Track: "v", Seq: 5, TS: 1 * Millisecond}, // rollback >500ms -> isolate
	}
	res := RepairTimeline(pts)
	if len(res.Isolated) != 1 || res.Isolated[0].Seq != 5 {
		t.Fatalf("isolated=%v want seq 5", res.Isolated)
	}
	if res.Repairs != 2 {
		t.Fatalf("repairs=%d want 2", res.Repairs)
	}
	if err := CheckMonotonic("v", collectTS(res.Repaired)); err != nil {
		t.Fatalf("repaired timeline not monotonic: %v", err)
	}
	if res.Repaired[1].TS != pts[0].TS+1 {
		t.Errorf("duplicate nudge wrong: %d", res.Repaired[1].TS)
	}
	if res.Errors[2].Code != CodeRollback || res.Errors[2].Action != ActionIsolated {
		t.Errorf("over-limit defect code=%s action=%s", res.Errors[2].Code, res.Errors[2].Action)
	}
}

func TestRepairBoundary(t *testing.T) {
	base := int64(1000 * Millisecond)
	// Exactly 500ms rollback: want = prev+1, delta = 500ms+1 -> over limit.
	pts := []Point{
		{Track: "a", Seq: 0, TS: base},
		{Track: "a", Seq: 1, TS: base - MaxRepairDelta},
	}
	res := RepairTimeline(pts)
	if len(res.Isolated) != 1 {
		t.Fatalf("500ms boundary must isolate, got %v", res.Repaired)
	}
	// One nanosecond inside the limit must be repairable.
	pts2 := []Point{
		{Track: "a", Seq: 0, TS: base},
		{Track: "a", Seq: 1, TS: base - MaxRepairDelta + 2},
	}
	res2 := RepairTimeline(pts2)
	if len(res2.Isolated) != 0 || res2.Repairs != 1 {
		t.Fatalf("sub-500ms rollback must be repaired, got %+v", res2)
	}
}

func TestMergePoints(t *testing.T) {
	a := []Point{{Track: "a", Seq: 0, TS: 5}, {Track: "a", Seq: 1, TS: 20}}
	b := []Point{{Track: "b", Seq: 0, TS: 5}, {Track: "b", Seq: 1, TS: 12}}
	merged := MergePoints(a, b)
	if err := CheckMonotonic("mux", collectTS(merged)); err != nil {
		t.Fatal(err)
	}
	if len(merged) != 4 || merged[0].TS != 5 || merged[1].TS != 6 {
		t.Errorf("merge nudge wrong: %+v", merged)
	}
}

func collectTS(ps []Point) []int64 {
	out := make([]int64, len(ps))
	for i, p := range ps {
		out[i] = p.TS
	}
	return out
}
