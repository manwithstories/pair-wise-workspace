package media

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestRationalReduceAndCompare(t *testing.T) {
	r := MustRat(90, 30)
	if r.String() != "3/1" {
		t.Fatalf("got %s", r)
	}
	if got := MustRat(1, 3).Cmp(MustRat(2, 6)); got != 0 {
		t.Fatalf("cmp equal: %d", got)
	}
	if got := MustRat(1, 3).Cmp(MustRat(1, 2)); got != -1 {
		t.Fatalf("cmp less: %d", got)
	}
	if got := MustRat(7, 4).Cmp(MustRat(5, 3)); got != 1 {
		t.Fatalf("cmp greater: %d", got)
	}
	if _, err := NewRat(1, 0); !errors.Is(err, ErrZeroDenominator) {
		t.Fatalf("expected zero denominator, got %v", err)
	}
	if v := MustRat(5, -10); v.String() != "-1/2" {
		t.Fatalf("negative denominator: %s", v)
	}
}

func TestRationalArithmeticExact(t *testing.T) {
	a := MustRat(1, 3)
	b := MustRat(1, 6)
	sum, err := a.Add(b)
	if err != nil || sum.String() != "1/2" {
		t.Fatalf("add: %s %v", sum, err)
	}
	diff, err := a.Sub(b)
	if err != nil || diff.String() != "1/6" {
		t.Fatalf("sub: %s %v", diff, err)
	}
	prod, err := a.Mul(b)
	if err != nil || prod.String() != "1/18" {
		t.Fatalf("mul: %s %v", prod, err)
	}
}

func TestParseDuration(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"2s", "2/1"},
		{"1500ms", "3/2"},
		{"1.5s", "3/2"},
		{"1/3", "1/3"},
		{" 2s ", "2/1"},
	} {
		got, err := ParseDuration(tc.in)
		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if got.String() != tc.want {
			t.Fatalf("%q: got %s want %s", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "-1s", "abc", "1/0", "1s2"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Fatalf("%q should fail", bad)
		}
	}
}

func TestTimeRescaleRoundTrip(t *testing.T) {
	base90k := MustRat(1, 90000)
	base30 := MustRat(1, 30)
	if v, err := Rescale(0, base90k, base90k); err != nil || v != 0 {
		t.Fatalf("identity: %d %v", v, err)
	}
	if v, err := Rescale(90000, base90k, base30); err != nil || v != 30 {
		t.Fatalf("90k->30: %d %v", v, err)
	}
	if v, err := Rescale(30, base30, base90k); err != nil || v != 90000 {
		t.Fatalf("30->90k: %d %v", v, err)
	}
	// 1/30 s is 3000 ticks at 90kHz exactly.
	if v, err := Rescale(1, base30, base90k); err != nil || v != 3000 {
		t.Fatalf("1 tick 30->90k: %d %v", v, err)
	}
}

func TestTimeRescaleRoundsNearest(t *testing.T) {
	src := MustRat(1, 1000000) // microsecond ticks
	dst := MustRat(1, 3)       // third-of-second ticks
	// 1 microsecond is 0.000003 of a third-second tick -> rounds to 0.
	if v, err := Rescale(1, src, dst); err != nil || v != 0 {
		t.Fatalf("round down: %d %v", v, err)
	}
	// 166667 us -> 0.500001 -> rounds to 1.
	if v, err := Rescale(166667, src, dst); err != nil || v != 1 {
		t.Fatalf("round half: %d %v", v, err)
	}
}

func TestTimeSecondsAndCompare(t *testing.T) {
	a := MustTime(33, MustRat(1, 30))
	b := MustTime(34, MustRat(1, 30))
	sec, err := a.Seconds()
	if err != nil || sec.String() != "11/10" {
		t.Fatalf("seconds: %s %v", sec, err)
	}
	if a.Cmp(b) != -1 || b.Cmp(a) != 1 || a.Cmp(a) != 0 {
		t.Fatal("cmp ordering")
	}
	d, err := b.Sub(a)
	if err != nil || d.String() != "1/30" {
		t.Fatalf("sub: %s %v", d, err)
	}
}

func TestTimeAddSeconds(t *testing.T) {
	base := MustRat(1, 45)
	t0 := MustTime(10, base)
	t1, err := t0.AddSeconds(MustRat(2, 1))
	if err != nil || t1.Ticks != 100 {
		t.Fatalf("add 2s: %+v %v", t1, err)
	}
	t2, err := t1.AddSeconds(MustRat(-1, 1))
	if err != nil || t2.Ticks != 55 {
		t.Fatalf("add -1s: %+v %v", t2, err)
	}
	if _, err := t1.AddSeconds(MustRat(math.MaxInt64, 1)); err == nil {
		t.Fatal("expected overflow")
	}
}

func TestParseDurationToGoDuration(t *testing.T) {
	d, err := MustRat(1, 3).Duration()
	if err != nil {
		t.Fatal(err)
	}
	if d < 333*time.Millisecond || d > 334*time.Millisecond {
		t.Fatalf("duration %v", d)
	}
}

func TestTimeRejectsZeroBase(t *testing.T) {
	if _, err := NewTime(1, MustRat(0, 1)); err == nil {
		t.Fatal("expected invalid base")
	}
	if _, err := NewTime(1, Rational{}); err == nil {
		t.Fatal("expected zero-value base rejection")
	}
}
