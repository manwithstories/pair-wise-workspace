package clock

import "testing"

func TestExtendCrossesSingleWrap(t *testing.T) {
	prev := Wrap - 100
	next, wrapped := Extend(prev, Stamp(50))
	if !wrapped {
		t.Fatalf("expected wrap crossing")
	}
	if next != Wrap+50 {
		t.Fatalf("extended = %d, want %d", next, Wrap+50)
	}
}

func TestExtendStaysOnSameEpochForSmallForwardStep(t *testing.T) {
	prev := uint64(1000)
	next, wrapped := Extend(prev, Stamp(5000))
	if wrapped {
		t.Fatalf("unexpected wrap")
	}
	if next != 5000 {
		t.Fatalf("extended = %d, want 5000", next)
	}
}

func TestExtendBackwardsJitterDoesNotWrap(t *testing.T) {
	prev := uint64(5000)
	next, wrapped := Extend(prev, Stamp(4900))
	if wrapped {
		t.Fatalf("small backwards jitter must not wrap")
	}
	if next != 4900 {
		t.Fatalf("extended = %d, want 4900", next)
	}
}

func TestExtenderMonotonicAcrossWraps(t *testing.T) {
	e := new(Extender)
	raw := []Stamp{4294967290, 4294967295, 5, 200, 4294967290, 1}
	var got []uint64
	for _, r := range raw {
		v, _ := e.Next(r)
		got = append(got, v)
	}
	last := uint64(0)
	for i, v := range got {
		if i > 0 && v < last {
			t.Fatalf("tick %d regressed: %d < %d", i, v, last)
		}
		last = v
	}
	if e.Wraps() != 2 {
		t.Fatalf("wraps = %d, want 2", e.Wraps())
	}
	if got[0] != 0 {
		t.Fatalf("first tick = %d, want 0", got[0])
	}
	if got[2] != 11 {
		t.Fatalf("tick after wrap = %d, want 11", got[2])
	}
}

func TestExtenderRestoreResumesSameTimeline(t *testing.T) {
	e := new(Extender)
	for _, r := range []Stamp{100, 200, 300} {
		e.Next(r)
	}
	origin, last, wraps := e.Origin(), e.Last(), e.Wraps()

	fresh := new(Extender)
	fresh.Restore(origin, last, wraps)
	v, wrapped := fresh.Next(400)
	if wrapped {
		t.Fatalf("unexpected wrap after restore")
	}
	if v != 300 {
		t.Fatalf("resumed tick = %d, want 300", v)
	}
}

func TestElapsedUses32BitDistance(t *testing.T) {
	if d := Elapsed(Stamp(4294967290), Stamp(5)); d != 11 {
		t.Fatalf("elapsed = %d, want 11", d)
	}
	if d := Elapsed(Stamp(10), Stamp(90)); d != 80 {
		t.Fatalf("elapsed = %d, want 80", d)
	}
}

func TestTicksAndDurationRoundTrip(t *testing.T) {
	if got := Ticks(0); got != 0 {
		t.Fatalf("Ticks(0) = %d", got)
	}
	if got := Ticks(1500e6); got != Rate*3/2 {
		t.Fatalf("Ticks(1.5s) = %d", got)
	}
	if got := Millis(Rate); got != 1000 {
		t.Fatalf("Millis(1s) = %d", got)
	}
	if got := Interval(10, 4); got != 0 {
		t.Fatalf("Interval backwards = %d", got)
	}
	if got := Interval(10, 25); got != 15 {
		t.Fatalf("Interval = %d, want 15", got)
	}
}
