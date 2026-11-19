package clock

import "testing"

func TestExtend32(t *testing.T) {
	cases := []struct {
		name string
		ref  uint64
		v    uint32
		want uint64
	}{
		{"plain forward", 1000, 1010, 1010},
		{"forward wrap", 0xFFFFFFF8, 4, 0x100000004},
		{"small backward no wrap", 1000, 990, 990},
		{"backward wrap", 0x100000004, 0xFFFFFFF8, 0xFFFFFFF8},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Extend(c.ref, c.v, false); got != c.want {
				t.Fatalf("Extend(%d,%d)=%d want %d", c.ref, c.v, got, c.want)
			}
		})
	}
}

func TestExtend16(t *testing.T) {
	if got := Extend(0xFFF8, 2, true); got != 0x10002 {
		t.Fatalf("seq wrap extend=%#x want 0x10002", got)
	}
	if got := Extend(0x10002, 0xFFF8, true); got != 0xFFF8 {
		t.Fatalf("seq backward extend=%#x want 0xfff8", got)
	}
}

func TestTimelineWrapAndAnomaly(t *testing.T) {
	tl, err := NewTimeline(48000, 0)
	if err != nil {
		t.Fatal(err)
	}
	tl.Anchor(0xFFFFFFF0, 0)
	// 16 ticks after wrap at 48 kHz = 333333 ns.
	ns, merr := tl.ToNS(0x00000000)
	if merr != nil {
		t.Fatalf("unexpected anomaly: %v", merr)
	}
	if ns <= 0 {
		t.Fatalf("wrap produced non-positive ns=%d", ns)
	}
	// A forward jump larger than 5 s at 48 kHz must be flagged.
	big := uint32(48000 * 6)
	_, merr = tl.ToNS(big)
	if merr == nil {
		t.Fatal("expected timestamp anomaly, got nil")
	}
}

func TestRoundTripTicksNS(t *testing.T) {
	tl, _ := NewTimeline(48000, 0)
	for _, ns := range []int64{0, 1_000_000, 333_333, 12_345_678} {
		ticks := tl.NSToTicks(ns)
		back := tl.ticksToNS(ticks)
		if d := back - ns; d < -15000 || d > 15000 {
			t.Fatalf("ns=%d ticks=%d back=%d drift=%d", ns, ticks, back, d)
		}
	}
}

func TestZeroClockRate(t *testing.T) {
	if _, err := NewTimeline(0, 0); err == nil {
		t.Fatal("expected error for zero clock rate")
	}
}
