package clock

import "testing"

func TestRationalAddNormalize(t *testing.T) {
	a := Must(1, 2)
	b := Must(1, 3)
	s, err := a.Add(b)
	if err != nil {
		t.Fatal(err)
	}
	if s.Num != 5 || s.Den != 6 {
		t.Fatalf("1/2+1/3 = %v, want 5/6", s)
	}
	if Must(2, 4) != Must(1, 2) {
		t.Fatal("normalization failed")
	}
	if _, err := New(1, 0); err == nil {
		t.Fatal("expected error for zero denominator")
	}
}

func TestRationalOverflow(t *testing.T) {
	big := Must(1<<62, 1)
	if _, err := big.Add(big); err != ErrOverflow {
		t.Fatalf("want ErrOverflow, got %v", err)
	}
}

func TestRationalCmpAndMicros(t *testing.T) {
	if Must(1, 3).Cmp(Must(2, 6)) != 0 {
		t.Fatal("1/3 != 2/6")
	}
	if Must(3, 2).Cmp(Must(4, 3)) != 1 {
		t.Fatal("3/2 should exceed 4/3")
	}
	us, err := Must(1, 48000).Micros()
	if err != nil || us != 20 {
		t.Fatalf("1/48000s = %dus, want 20 (%v)", us, err)
	}
}

func TestSignedDelta(t *testing.T) {
	const p = 1 << 33
	if d := SignedDelta(10, p-5, p); d != 15 {
		t.Fatalf("forward wrap delta = %d, want 15", d)
	}
	if d := SignedDelta(p-5, 10, p); d != -15 {
		t.Fatalf("backward wrap delta = %d, want -15", d)
	}
	if d := SignedDelta(3, 3, p); d != 0 {
		t.Fatalf("zero delta = %d", d)
	}
}

func TestUnwrap33Bit(t *testing.T) {
	u := &Unwrap{Period: 1 << 33}
	base := uint64(1<<33) - 180000
	var prev int64
	for i := uint64(0); i < 100; i++ {
		raw := (base + i*3600) % (1 << 33)
		got := u.Next(raw)
		want := int64(base) + int64(i)*3600
		if got != want {
			t.Fatalf("sample %d: got %d want %d", i, got, want)
		}
		prev = got
	}
	if prev <= 1<<33 {
		t.Fatalf("expected unwrapped timeline past wrap, got %d", prev)
	}
}

func TestUnwrapJitterNoFalseWrap(t *testing.T) {
	u := &Unwrap{Period: 1 << 32}
	seq := []uint64{100, 90, 110, 105, 120} // small reorder jitter
	want := []int64{100, 90, 110, 105, 120}
	for i, raw := range seq {
		if got := u.Next(raw); got != want[i] {
			t.Fatalf("sample %d: got %d want %d", i, got, want[i])
		}
	}
}

func TestWrapPeriod(t *testing.T) {
	if WrapPeriod(90000) != 1<<33 {
		t.Fatal("90kHz clock must use 33-bit period")
	}
	if WrapPeriod(48000) != 1<<32 {
		t.Fatal("other clocks must use 32-bit period")
	}
}

func TestScale(t *testing.T) {
	cases := []struct {
		r    Rational
		ts   int64
		want int64
	}{
		{Must(2, 1), 90000, 180000},
		{Must(1, 48000), 48000, 1},
		{Must(19, 48000), 48000, 19},
		{Must(-1, 3), 3, -1}, // -1 rounds away from zero
		{Must(1, 7), 3, 0},   // 3/7 rounds to 0
	}
	for _, c := range cases {
		got, err := c.r.Scale(c.ts)
		if err != nil || got != c.want {
			t.Errorf("%v.Scale(%d) = %d, %v; want %d", c.r, c.ts, got, err, c.want)
		}
	}
	if _, err := Must(1<<62, 1).Scale(1 << 20); err != ErrOverflow {
		t.Fatal("expected overflow")
	}
	if _, err := Must(1, 1).Scale(0); err == nil {
		t.Fatal("zero timescale must be rejected")
	}
}
