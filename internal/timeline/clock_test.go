package timeline

import "testing"

func TestUnwrapperWrapsMod33(t *testing.T) {
	u := NewUnwrapper()
	base := uint64(Mod33 - 9000) // 100 ms before the wrap point
	t1, g1 := u.Extend(base)
	if g1 {
		t.Fatal("first stamp cannot be a gap")
	}
	t2, g2 := u.Extend(9000) // 100 ms after the wrap point
	if g2 {
		t.Fatalf("legal wrap must not be a gap, got gap at %d", t2)
	}
	if d := t2 - t1; d != 18000 {
		t.Fatalf("wrap delta = %d, want 18000 ticks", d)
	}
	if u.Gaps() != 0 {
		t.Fatalf("gaps = %d, want 0", u.Gaps())
	}
}

func TestUnwrapperFlagsBackwardJump(t *testing.T) {
	u := NewUnwrapper()
	u.Extend(1_000_000)
	_, gap := u.Extend(500_000) // 0.5 s backward, not a wrap
	if !gap {
		t.Fatal("backward jump must be reported as gap")
	}
	if u.Gaps() != 1 {
		t.Fatalf("gaps = %d, want 1", u.Gaps())
	}
	// Re-anchor: later monotonic stamps continue without inventing time.
	t3, gap3 := u.Extend(509_000)
	if gap3 || t3 <= 500_000 {
		t.Fatalf("re-anchor failed: t=%d gap=%v", t3, gap3)
	}
}

func TestUnwrapperFlagsHugeForwardJump(t *testing.T) {
	u := NewUnwrapper()
	u.Extend(0)
	_, gap := u.Extend(uint64(MaxForwardJump) + 45_000)
	if !gap {
		t.Fatal("forward jump beyond tolerance must be a gap, not a guess")
	}
}

func TestPCR27DiscardsExtension(t *testing.T) {
	c := NewClock()
	got, gap := c.ExtendPCR27(90_000*300 + 127)
	if gap || got != 90_000 {
		t.Fatalf("ExtendPCR27 = %d gap=%v, want 90000 false", got, gap)
	}
}
