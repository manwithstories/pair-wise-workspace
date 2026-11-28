package core

import "testing"

func TestFromUnitsExactFrameDurations(t *testing.T) {
	cases := []struct {
		fps  Rational
		want int64
	}{
		{Rat(1, 24), 3750},
		{Rat(1, 25), 3600},
		{Rat(1, 30), 3000},
		{Rat(Rat(1001, 30000).Num, Rat(1001, 30000).Den), 3003},
	}
	for _, c := range cases {
		if got := FromUnits(1, c.fps).Ticks; got != c.want {
			t.Errorf("frame duration of %+v = %d ticks, want %d", c.fps, got, c.want)
		}
	}
}

func TestNoDriftOverLongAccumulation(t *testing.T) {
	// 30000/1001 fps: 30000 frames must span exactly 1001 seconds.
	frame := Rat(1001, 30000)
	frames := int64(30000)
	var acc Time
	step := FromUnits(1, frame)
	for i := int64(0); i < frames; i++ {
		acc = acc.Add(step)
	}
	if acc.Ticks != 3003*frames {
		t.Fatalf("accumulated %d ticks, want %d", acc.Ticks, 3003*frames)
	}
	if acc.Millis() != 1001000 {
		t.Fatalf("30000 frames at 29.97 = %d ms, want 1001000", acc.Millis())
	}
}

func TestTimeArithmeticAndCompare(t *testing.T) {
	a := FromMillis(1500)
	b := FromMillis(500)
	if a.Sub(b).Millis() != 1000 {
		t.Errorf("sub: got %d ms", a.Sub(b).Millis())
	}
	if a.Cmp(b) != 1 || b.Cmp(a) != -1 || a.Cmp(a) != 0 {
		t.Error("cmp inconsistent")
	}
	if a.Add(b).Millis() != 2000 {
		t.Errorf("add: got %d ms", a.Add(b).Millis())
	}
}

func TestStringFormatsNegative(t *testing.T) {
	if got := FromMillis(-61234).String(); got != "-00:01:01.234" {
		t.Errorf("got %q", got)
	}
	if got := FromMillis(3723004).String(); got != "01:02:03.004" {
		t.Errorf("got %q", got)
	}
}

func TestFaultsAggregation(t *testing.T) {
	fs := Faults{
		NewFault(FaultBadCue, 10, true, "x"),
		NewFault(FaultCorruptMedia, 99, false, "y"),
		NewFault(FaultBadCue, 20, true, "z"),
	}
	if !fs.HasUnrecoverable() {
		t.Error("expected unrecoverable")
	}
	if n := len(fs.OfKind(FaultBadCue)); n != 2 {
		t.Errorf("OfKind = %d, want 2", n)
	}
	if (Fault{}).Error() == "" {
		t.Error("Error() must not be empty")
	}
}
