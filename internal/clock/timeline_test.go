package clock

import "testing"

func TestTimelineAnchorsOnFirstSegment(t *testing.T) {
	tl := NewTimeline(90000)
	d := tl.ResolveTFDT(true, 12345)
	if d.Repaired || d.Undetermined || d.Value != 12345 {
		t.Fatalf("unexpected decision: %+v", d)
	}
	tl.Advance(90000)
	if next, ok := tl.Next(); !ok || next != 102345 {
		t.Fatalf("next=%d ok=%v", next, ok)
	}
}

func TestTimelineRepairsRollback(t *testing.T) {
	tl := NewTimeline(48000)
	tl.ResolveTFDT(true, 0)
	tl.Advance(48000)
	d := tl.ResolveTFDT(true, 9600) // rolled back
	if !d.Repaired || d.Value != 48000 || d.Original != 9600 {
		t.Fatalf("rollback not repaired: %+v", d)
	}
}

func TestTimelineAdoptsForwardLeap(t *testing.T) {
	tl := NewTimeline(90000)
	tl.ResolveTFDT(true, 0)
	tl.Advance(90000)
	d := tl.ResolveTFDT(true, 180000)
	if d.Repaired || d.Value != 180000 {
		t.Fatalf("forward leap should be adopted: %+v", d)
	}
}

func TestTimelineMissingTFDT(t *testing.T) {
	tl := NewTimeline(90000)
	d := tl.ResolveTFDT(false, 0)
	if !d.Undetermined {
		t.Fatalf("first segment without tfdt must be undetermined: %+v", d)
	}
	tl.ResolveTFDT(true, 0)
	tl.Advance(90000)
	d = tl.ResolveTFDT(false, 0)
	if !d.Repaired || d.Value != 90000 {
		t.Fatalf("missing tfdt should repair to expected: %+v", d)
	}
}

func TestTimelineSkipKeepsContinuity(t *testing.T) {
	tl := NewTimeline(90000)
	tl.ResolveTFDT(true, 0)
	tl.Advance(90000)
	tl.Skip(90000) // lost segment
	d := tl.ResolveTFDT(true, 180000)
	if d.Repaired || d.Value != 180000 {
		t.Fatalf("after skip the next segment should anchor cleanly: %+v", d)
	}
}

func TestRescale(t *testing.T) {
	if got := Rescale(90000, 90000, MicrosPerSecond); got != 1_000_000 {
		t.Fatalf("rescale=%d", got)
	}
	if got := ToMicros(48000, 48000); got != 1_000_000 {
		t.Fatalf("toMicros=%d", got)
	}
	if got := Rescale(-45, 90, 1_000_000); got != -500000 {
		t.Fatalf("negative rescale=%d", got)
	}
	if got := Rescale(1, 3, 1); got != 0 {
		t.Fatalf("rounding rescale=%d", got)
	}
}

func TestFaultError(t *testing.T) {
	f := NewFault(FaultTruncate, 7, 0, 512, "box exceeds parent bounds")
	want := "truncate seq=7 offset=512: box exceeds parent bounds"
	if f.Error() != want {
		t.Fatalf("got %q want %q", f.Error(), want)
	}
}
