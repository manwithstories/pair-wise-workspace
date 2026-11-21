package clock

import "testing"

func TestLinearProgress(t *testing.T) {
	var tl Timeline
	for i := uint64(0); i < 1000; i += 100 {
		ev := tl.Unwrap(i)
		if ev.Value != int64(i) || ev.Wrapped || ev.Backward {
			t.Fatalf("raw=%d got %+v", i, ev)
		}
	}
	if tl.Wraps() != 0 {
		t.Fatalf("wraps=%d want 0", tl.Wraps())
	}
}

func TestSingleWrap(t *testing.T) {
	var tl Timeline
	start := Modulus - 500
	if ev := tl.Unwrap(start); ev.Value != int64(start) {
		t.Fatalf("seed got %d want %d", ev.Value, start)
	}
	ev := tl.Unwrap(499) // wrapped forward by 1000 ticks
	if !ev.Wrapped {
		t.Fatal("wrap not detected")
	}
	if ev.Value != int64(Modulus+499) {
		t.Fatalf("unwrapped=%d want %d", ev.Value, Modulus+499)
	}
	if tl.Wraps() != 1 {
		t.Fatalf("wraps=%d want 1", tl.Wraps())
	}
	// Continue past the wrap: still monotonic.
	ev = tl.Unwrap(1500)
	if ev.Value != int64(Modulus+1500) || ev.Wrapped {
		t.Fatalf("post-wrap got %+v", ev)
	}
}

func TestBackwardJumpDoesNotRebase(t *testing.T) {
	var tl Timeline
	tl.Unwrap(1000)
	tl.Unwrap(2000)
	ev := tl.Unwrap(500) // non-monotonic jump
	if !ev.Backward {
		t.Fatal("backward jump not flagged")
	}
	if ev.Value != 500 {
		t.Fatalf("value=%d want 500", ev.Value)
	}
	// Recovery continues from the pre-jump reference, not from 500.
	ev = tl.Unwrap(3000)
	if ev.Backward || ev.Value != 3000 {
		t.Fatalf("recovery got %+v", ev)
	}
}

func TestDoubleWrap(t *testing.T) {
	var tl Timeline
	pos := uint64(0)
	step := uint64(1 << 30) // ~1/8 of the modulus, well under half
	tl.Unwrap(pos)
	wraps := 0
	for i := 0; i < 20; i++ {
		pos = (pos + step) & (Modulus - 1)
		if tl.Unwrap(pos).Wrapped {
			wraps++
		}
	}
	if wraps != tl.Wraps() || wraps < 2 {
		t.Fatalf("wraps=%d counted=%d", tl.Wraps(), wraps)
	}
}
