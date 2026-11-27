package clock

import "testing"

func TestRatFloorCeil(t *testing.T) {
	r := Rat{Num: 30000, Den: 1001 * Second}
	// Frame 0 must stay zero under both operations.
	if r.FloorDiv(0) != 0 || r.CeilDiv(0) != 0 {
		t.Fatal("zero timestamp must map to frame 0")
	}
	// A nanosecond strictly inside frame 0 floors to 0 and ceils to 1.
	ns1 := framesToNanos(Rate2997, 1) // exact PTS of frame 1
	if got := r.CeilDiv(ns1); got != 1 {
		t.Errorf("ceil(PTS(frame1))=%d want 1", got)
	}
	if got := r.FloorDiv(ns1 - 1); got != 0 {
		t.Errorf("floor(PTS-1)=%d want 0", got)
	}
}

func TestTimecodeString(t *testing.T) {
	if got := (Timecode{Hour: 0, Min: 1, Sec: 2, Frame: 3, Rate: Rate25}).String(); got != "00:01:02:03" {
		t.Errorf("NDF string = %q", got)
	}
	if got := (Timecode{Hour: 0, Min: 1, Sec: 2, Frame: 3, Rate: Rate2997DF}).String(); got != "00:01:02;03" {
		t.Errorf("DF string = %q", got)
	}
}

func TestErrorWrapping(t *testing.T) {
	e := NewError(CodeRollback, "v", Interval{Start: 1, End: 2}, "boom")
	if e.Action != ActionNone {
		t.Fatal("default action must be none")
	}
	e2 := e.WithAction(ActionShifted)
	if e2.Action != ActionShifted || e.Action != ActionNone {
		t.Fatal("WithAction must not mutate the receiver")
	}
	if !HasCode(e2, CodeRollback) || HasCode(e2, CodeChecksum) {
		t.Fatal("HasCode classification wrong")
	}
	if e2.Error() == "" {
		t.Fatal("Error() must render a message")
	}
}
