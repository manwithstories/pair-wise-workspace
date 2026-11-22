package clock

import "testing"

func TestNewRejects(t *testing.T) {
	if _, err := New(0, 30); err == nil {
		t.Fatal("zero rate must be rejected")
	}
	if _, err := New(90000, 0); err == nil {
		t.Fatal("zero fps must be rejected")
	}
	if _, err := New(90001, 30); err == nil {
		t.Fatal("non-divisible rate must be rejected")
	}
}

func TestFrameTicksAndMillis(t *testing.T) {
	c, err := New(90000, 30)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.TicksPerFrame(); got != 3000 {
		t.Fatalf("ticks/frame = %d, want 3000", got)
	}
	ticks, err := c.FrameTicks(30)
	if err != nil || ticks != 90000 {
		t.Fatalf("frame 30 ticks = %d, %v", ticks, err)
	}
	if got := c.Millis(ticks); got != 1000 {
		t.Fatalf("millis = %d, want 1000", got)
	}
	back, err := c.FromMillis(1000)
	if err != nil || back != 90000 {
		t.Fatalf("fromMillis = %d, %v", back, err)
	}
}

func TestWrapAndDelta32(t *testing.T) {
	if Wrap32(RTPModulus+7) != 7 {
		t.Fatal("wrap folding broken")
	}
	// forward wrap across the 32-bit boundary
	if got := Delta32(0xFFFFFFF0, 0x00000010); got != 0x20 {
		t.Fatalf("forward wrap delta = %d, want 32", got)
	}
	// late packet just before the wrap boundary
	if got := Delta32(0x00000010, 0xFFFFFFF0); got != -0x20 {
		t.Fatalf("backward wrap delta = %d, want -32", got)
	}
}

func TestExpand32(t *testing.T) {
	prev := int64(0xFFFFFFF0)
	if got := Expand32(prev, 0x00000010); got != int64(0x100000010) {
		t.Fatalf("expand forward wrap = %#x", got)
	}
	// late arrival before the wrap
	if got := Expand32(prev, 0xFFFFFFE0); got != int64(0xFFFFFFE0) {
		t.Fatalf("expand late = %#x", got)
	}
}

func TestOverflowGuard(t *testing.T) {
	c, _ := New(90000, 30)
	if _, err := c.FrameTicks(-1); err == nil {
		t.Fatal("negative index rejected")
	}
	if _, err := c.FrameTicks(1 << 58); err != ErrOverflow {
		t.Fatalf("overflow guard, got %v", err)
	}
}
