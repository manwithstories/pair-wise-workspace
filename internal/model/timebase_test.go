package model

import "testing"

func TestNewTimebaseNormalizes(t *testing.T) {
	tb, err := NewTimebase(60000, 2002)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if tb.String() != "30000/1001" {
		t.Fatalf("timebase = %s, want 30000/1001", tb)
	}
	if _, err := NewTimebase(1, 0); err == nil {
		t.Fatal("zero denominator must be rejected")
	}
	if _, err := NewTimebase(0, 1); err == nil {
		t.Fatal("zero numerator must be rejected")
	}
	// A negative denominator flips the sign; a non-positive rate is invalid.
	if _, err := NewTimebase(1, -2); err == nil {
		t.Fatal("1/-2 normalises to a negative rate and must be rejected")
	}
	if tb, err := NewTimebase(-1, -2); err != nil || tb.String() != "1/2" {
		t.Fatalf("-1/-2 should normalise to 1/2, got %v / %v", tb, err)
	}
}

func TestConvertToIsExactForRationalRates(t *testing.T) {
	// A 30000/1001 tick equals 1001/30000 s, so converting to 90 kHz multiplies
	// by 90000*1001/30000 = 3003, exactly for every whole tick.
	ntsc := MustTimebase(30000, 1001)
	for _, ticks := range []int64{0, 1, 1001, 3003, 165150} {
		got, err := ntsc.ConvertTo(ticks, RTP90k)
		if err != nil {
			t.Fatalf("convert %d: %v", ticks, err)
		}
		if want := ticks * 3003; got != want {
			t.Fatalf("convert %d = %d, want %d", ticks, got, want)
		}
	}
}

func TestConvertToRoundsHalfToEven(t *testing.T) {
	// 1/2 tick in the target rate: 0.5 rounds to 0, 1.5 rounds to 2.
	tb := MustTimebase(2, 1)
	dst := MustTimebase(1, 1)
	for _, c := range []struct{ in, want int64 }{
		{0, 0}, {1, 0}, {2, 1}, {3, 2}, {5, 2}, {7, 4},
	} {
		got, err := tb.ConvertTo(c.in, dst)
		if err != nil {
			t.Fatalf("convert %d: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("convert %d = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestConvertToDetectsOverflow(t *testing.T) {
	tb := MustTimebase(1, 1)
	dst := MustTimebase(1<<62, 1)
	if _, err := tb.ConvertTo(1<<40, dst); err == nil {
		t.Fatal("expected overflow error")
	}
}

func TestUnwrapSequence(t *testing.T) {
	raws := []uint32{0xfffffff0, 0xfffffff8, 0x00000000, 0x00000008, 0xffffffff, 0x00000001}
	ticks, epochs, err := UnwrapSequence(raws, 0)
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	for i := 1; i < len(ticks); i++ {
		if ticks[i] <= ticks[i-1] {
			t.Fatalf("ticks not increasing at %d: %v", i, ticks)
		}
	}
	if got := CountWraps(epochs); got != 2 {
		t.Fatalf("wraps = %d, want 2", got)
	}
	// The first tick must be the raw value in epoch zero.
	if ticks[0] != int64(raws[0]) {
		t.Fatalf("first tick = %d, want %d", ticks[0], raws[0])
	}
}

func TestExpandWrap(t *testing.T) {
	got, err := ExpandWrap(5, 2)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if want := int64(2)<<32 + 5; got != want {
		t.Fatalf("expand = %d, want %d", got, want)
	}
	if _, err := ExpandWrap(0, -1); err == nil {
		t.Fatal("negative epoch must be rejected")
	}
}

func TestParseTimebase(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"90000", "90000/1"},
		{"30000/1001", "30000/1001"},
		{" 25 / 1 ", "25/1"},
	} {
		tb, err := ParseTimebase(c.in)
		if err != nil {
			t.Fatalf("parse %q: %v", c.in, err)
		}
		if tb.String() != c.want {
			t.Fatalf("parse %q = %s, want %s", c.in, tb, c.want)
		}
	}
	if _, err := ParseTimebase(""); err == nil {
		t.Fatal("empty string must be rejected")
	}
	if _, err := ParseTimebase("nope"); err == nil {
		t.Fatal("non-numeric rate must be rejected")
	}
}

func TestSecondsString(t *testing.T) {
	if got := RTP90k.SecondsString(90000); got != "1.000000" {
		t.Fatalf("seconds = %s, want 1.000000", got)
	}
	if got := RTP90k.SecondsString(45000); got != "0.500000" {
		t.Fatalf("seconds = %s, want 0.500000", got)
	}
}

func TestParseRect(t *testing.T) {
	r, err := ParseRect("1,2,3,4")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r != (Rect{Left: 1, Top: 2, Right: 3, Bottom: 4}) {
		t.Fatalf("rect = %+v", r)
	}
	if _, err := ParseRect("1,2,3"); err == nil {
		t.Fatal("short rect must be rejected")
	}
	if _, err := ParseRect("1,2,3,-4"); err == nil {
		t.Fatal("negative inset must be rejected")
	}
}
