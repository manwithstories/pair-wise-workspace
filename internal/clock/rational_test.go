package clock

import (
	"math"
	"testing"
)

func TestNewCanonicalizes(t *testing.T) {
	cases := []struct {
		n, d int64
		want Rational
	}{
		{6, -4, Rational{-3, 2}},
		{0, 7, Rational{0, 1}},
		{-9, 3, Rational{-3, 1}},
		{30000, 1001, Rational{30000, 1001}},
	}
	for _, c := range cases {
		got, err := New(c.n, c.d)
		if err != nil {
			t.Fatalf("New(%d,%d): %v", c.n, c.d, err)
		}
		if got != c.want {
			t.Errorf("New(%d,%d)=%v want %v", c.n, c.d, got, c.want)
		}
	}
	if _, err := New(1, 0); err != ErrZeroDenominator {
		t.Fatalf("zero denominator: got %v", err)
	}
}

func TestParse(t *testing.T) {
	cases := map[string]Rational{
		"25":         {25, 1},
		"30000/1001": {30000, 1001},
		"1.5":        {3, 2},
		"-0.001":     {-1, 1000},
	}
	for in, want := range cases {
		got, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("Parse(%q)=%v want %v", in, got, want)
		}
	}
	if _, err := Parse("x/y"); err == nil {
		t.Error("expected malformed rational error")
	}
}

func TestArithmeticExact(t *testing.T) {
	frame := MustNew(1, 30)
	total := Zero()
	for i := 0; i < 300; i++ {
		var err error
		total, err = total.Add(frame)
		if err != nil {
			t.Fatal(err)
		}
	}
	if total != FromInt(10) {
		t.Fatalf("300 frames at 30fps = %v, want 10", total)
	}
	ms, err := total.Rescale(1000)
	if err != nil || ms != 10000 {
		t.Fatalf("Rescale = %d, %v; want 10000", ms, err)
	}
}

func TestRescaleRounding(t *testing.T) {
	cases := []struct {
		r    Rational
		unit int64
		want int64
	}{
		{MustNew(1, 3), 1000, 333},
		{MustNew(2, 3), 1000, 667},
		{MustNew(-1, 3), 1000, -333},
		{MustNew(30000, 1001), 1000, 29970},
	}
	for _, c := range cases {
		got, err := c.r.Rescale(c.unit)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%v rescale %d = %d want %d", c.r, c.unit, got, c.want)
		}
	}
}

func TestRoundAndTrunc(t *testing.T) {
	if got := MustNew(7, 2).Round(); got != 4 {
		t.Errorf("7/2 round = %d", got)
	}
	if got := MustNew(-7, 2).Round(); got != -4 {
		t.Errorf("-7/2 round = %d", got)
	}
	if got := MustNew(-7, 2).Trunc(); got != -3 {
		t.Errorf("-7/2 trunc = %d", got)
	}
}

func TestFloatAndString(t *testing.T) {
	r := MustNew(30000, 1001)
	if math.Abs(r.Float64()-29.97002997) > 1e-8 {
		t.Errorf("float=%v", r.Float64())
	}
	if got := r.String(); got != "30000/1001" {
		t.Errorf("String=%q", got)
	}
	if got := FromInt(3).String(); got != "3" {
		t.Errorf("String=%q", got)
	}
}

func TestOverflow(t *testing.T) {
	big := Rational{Num: math.MaxInt64, Den: 1}
	if _, err := big.Mul(big); err != ErrOverflow {
		t.Fatalf("expected overflow, got %v", err)
	}
}
