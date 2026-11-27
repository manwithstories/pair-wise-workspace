package clock

import "testing"

func TestUnwrap64BasicAndWrap(t *testing.T) {
	u := NewUnwrap64(8) // tiny modulus keeps tests readable
	cases := []struct {
		raw   uint64
		want  uint64
		wraps int
	}{
		{250, 250, 0},
		{255, 255, 0},
		{0, 256, 1},   // forward wrap 255 -> 0
		{1, 257, 1},
		{255, 255, 0}, // backward wrap 1 -> 255
	}
	for _, c := range cases {
		got, w := u.Extend(c.raw)
		if got != c.want || w != c.wraps {
			t.Fatalf("Extend(%d) = %d,w%d want %d,w%d", c.raw, got, w, c.want, c.wraps)
		}
	}
}

func TestUnwrap64HalfWindow(t *testing.T) {
	u := NewUnwrap64(8)
	u.Extend(100)
	// +128 equals the half window boundary: treated as forward step.
	if got, _ := u.Extend(228); got != 228 {
		t.Fatalf("half-window forward = %d want 228", got)
	}
	// A small backward step inside the window stays a plain negative delta.
	u2 := NewUnwrap64(8)
	u2.Extend(5)
	if got, w := u2.Extend(2); got != 2 || w != 0 {
		t.Fatalf("small backward = %d,w%d want 2,0", got, w)
	}
	// Approaching the modulus from below then crossing it is a forward wrap.
	u3 := NewUnwrap64(8)
	u3.Extend(250)
	u3.Extend(255)
	if got, w := u3.Extend(0); got != 256 || w != 1 {
		t.Fatalf("forward wrap = %d,w%d want 256,1", got, w)
	}
}

func TestUnwrap33Bit(tf *testing.T) {
	u := NewUnwrap64(33)
	const mod = uint64(1) << 33
	base := mod - 3
	u.Extend(base)
	got, w := u.Extend(2)
	if got != mod+2 || w != 1 {
		tf.Fatalf("33-bit wrap = %d,w%d want %d,1", got, w, mod+2)
	}
}

func TestUnwrap16Ring(t *testing.T) {
	u := NewUnwrap16()
	if got := u.Extend(65534); got != 65534 {
		t.Fatalf("seed = %d", got)
	}
	if got := u.Extend(65535); got != 65535 {
		t.Fatalf("65535 = %d", got)
	}
	if got := u.Extend(0); got != 65536 {
		t.Fatalf("wrap to 0 = %d want 65536", got)
	}
	if got := u.Extend(1); got != 65537 {
		t.Fatalf("1 = %d want 65537", got)
	}
}

func TestSignedDelta16(t *testing.T) {
	cases := []struct {
		prev, cur uint16
		want      int32
	}{
		{10, 12, 2},
		{12, 10, -2},
		{65535, 0, 1},
		{0, 65535, -1},
		{0, 32767, 32767},
	}
	for _, c := range cases {
		if got := SignedDelta16(c.prev, c.cur); got != c.want {
			t.Fatalf("delta(%d,%d)=%d want %d", c.prev, c.cur, got, c.want)
		}
	}
}

func TestUnwrapReset(t *testing.T) {
	u := NewUnwrap64(8)
	u.Extend(255)
	u.Extend(0)
	u.Reset()
	if got, w := u.Extend(7); got != 7 || w != 0 {
		t.Fatalf("after reset = %d,w%d", got, w)
	}
}
