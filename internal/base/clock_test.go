package base

import "testing"

func TestClockWrapSeq(t *testing.T) {
	t.Parallel()
	c := NewClock()
	period := WrapPeriod
	pre := []uint64{period - 6000, period - 3000, period - 1000}
	post := []uint64{500, 1000, 1500, 2000}
	var last uint64
	first := true
	for _, r := range pre {
		out, gap := c.Advance(r)
		if gap {
			t.Fatalf("unexpected gap in pre-wrap sequence at %d", r)
		}
		if !first && out <= last {
			t.Fatalf("non-monotonic pre-wrap: %d <= %d", out, last)
		}
		last, first = out, false
	}
	for _, r := range post {
		out, gap := c.Advance(r)
		if gap {
			t.Fatalf("wrap should resolve without a gap at %d", r)
		}
		if out <= last {
			t.Fatalf("non-monotonic after wrap: %d <= %d", out, last)
		}
		last = out
	}
	snap := c.Snapshot()
	if snap.WrapCount == 0 {
		t.Fatal("expected at least one wrap to be recorded")
	}
}

func TestClockBackwardsJitterMonotonic(t *testing.T) {
	t.Parallel()
	c := NewClock()
	c.Advance(1000)
	first, _ := c.Advance(2000)
	back, _ := c.Advance(1900) // small backwards step, inside MaxBackStep
	if back <= first {
		t.Fatalf("backwards jitter must still advance output: %d <= %d", back, first)
	}
}

func TestClockEncoderRestart(t *testing.T) {
	t.Parallel()
	c := NewClock()
	c.Advance(100000)
	prev, _ := c.Advance(200000)
	// Large backwards jump larger than MaxBackStep (45k ticks) but under half
	// the modulus: an encoder restart, not a wrap.
	out, _ := c.Advance(100000)
	if out <= prev {
		t.Fatalf("restart must keep output monotonic: %d <= %d", out, prev)
	}
	if c.Snapshot().RestartCount == 0 {
		t.Fatal("expected a restart event")
	}
}

func TestClockForwardGapMarked(t *testing.T) {
	t.Parallel()
	c := NewClock()
	c.Advance(0)
	prev, _ := c.Advance(1000)
	// Enormous forward jump: unresolvable, must flag a gap but stay monotonic.
	out, gap := c.Advance(1000 + ClockRate*120)
	if !gap {
		t.Fatal("expected a clock gap flag")
	}
	if out <= prev {
		t.Fatalf("gap output must remain strictly increasing: %d <= %d", out, prev)
	}
	if c.Snapshot().GapCount == 0 {
		t.Fatal("expected gap accounting")
	}
}

func TestClockDuplicateTimestamp(t *testing.T) {
	t.Parallel()
	c := NewClock()
	a, _ := c.Advance(3000)
	b, _ := c.Advance(3000)
	if b <= a {
		t.Fatalf("duplicate raw PTS must still produce a later output: %d <= %d", b, a)
	}
}

func TestClockNormalizesNativeWidth(t *testing.T) {
	t.Parallel()
	c := NewClock()
	// A value wider than 33 bits must be folded into the modular field.
	out, _ := c.Advance(WrapPeriod + 1234)
	if out != 0 {
		t.Fatalf("first normalized output should start at zero, got %d", out)
	}
}
