package clock

import (
	"errors"
	"math"
	"testing"
)

func TestNewReducesAndNormalizesSign(t *testing.T) {
	cases := []struct {
		num, den int64
		wantNum  int64
		wantDen  int64
	}{
		{6, 4, 3, 2},
		{-6, 4, -3, 2},
		{6, -4, -3, 2},
		{-6, -4, 3, 2},
		{0, 7, 0, 1},
		{90000, 90000, 1, 1},
	}
	for _, c := range cases {
		got, err := New(c.num, c.den)
		if err != nil {
			t.Fatalf("New(%d,%d): %v", c.num, c.den, err)
		}
		if got.Num() != c.wantNum || got.Den() != c.wantDen {
			t.Errorf("New(%d,%d)=%s want %d/%d", c.num, c.den, got, c.wantNum, c.wantDen)
		}
	}
}

func TestNewRejectsZeroDenominator(t *testing.T) {
	if _, err := New(1, 0); !errors.Is(err, ErrZeroDenominator) {
		t.Fatalf("want ErrZeroDenominator, got %v", err)
	}
}

func TestArithmeticIsExact(t *testing.T) {
	a := MustNew(1, 3)
	b := MustNew(1, 6)
	sum, err := a.Add(b)
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Equal(MustNew(1, 2)) {
		t.Fatalf("1/3+1/6=%s want 1/2", sum)
	}
	d, err := a.Sub(b)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Equal(MustNew(1, 6)) {
		t.Fatalf("1/3-1/6=%s want 1/6", d)
	}
	m, err := a.Mul(MustNew(3, 1))
	if err != nil {
		t.Fatal(err)
	}
	if !m.Equal(MustNew(1, 1)) {
		t.Fatalf("1/3*3=%s want 1", m)
	}
	q, err := a.Div(b)
	if err != nil {
		t.Fatal(err)
	}
	if !q.Equal(MustNew(2, 1)) {
		t.Fatalf("1/3 / 1/6=%s want 2", q)
	}
	if _, err := a.Div(MustNew(0, 1)); !errors.Is(err, ErrZeroDenominator) {
		t.Fatalf("divide by zero: %v", err)
	}
}

func TestArithmeticRejectsOverflow(t *testing.T) {
	big := MustNew(math.MaxInt64, 1)
	if _, err := big.Add(MustNew(1, 1)); !errors.Is(err, ErrOverflow) {
		t.Fatalf("add overflow: %v", err)
	}
	if _, err := big.Mul(MustNew(2, 1)); !errors.Is(err, ErrOverflow) {
		t.Fatalf("mul overflow: %v", err)
	}
	if _, err := New(math.MaxInt64, 2); err != nil {
		t.Fatalf("max/2 should reduce fine: %v", err)
	}
	// A negative denominator forces a sign flip that cannot fit in int64.
	if _, err := New(math.MinInt64, -1); !errors.Is(err, ErrOverflow) {
		t.Fatalf("want overflow for MinInt64/-1, got %v", err)
	}
}

func TestComparisonsDoNotOverflow(t *testing.T) {
	// These cross products greatly exceed int64 but compare exactly.
	a := MustNew(math.MaxInt64, 1)
	b := MustNew(1, math.MaxInt64)
	if a.Cmp(b) <= 0 {
		t.Fatalf("max should exceed 1/max")
	}
	if !b.Abs().Equal(b) {
		t.Fatalf("abs of positive changed value")
	}
	if a.Neg().Sign() != -1 {
		t.Fatalf("neg sign")
	}
}

func TestConvertExact(t *testing.T) {
	got, err := ConvertExact(3, 90000, 90000)
	if err != nil || got != 3 {
		t.Fatalf("identity convert = %d, %v", got, err)
	}
	got, err = ConvertExact(180000, 90000, 48000)
	if err != nil || got != 96000 {
		t.Fatalf("90000->48000 = %d, %v", got, err)
	}
	if _, err := ConvertExact(1, 90000, 48000); !errors.Is(err, ErrInexact) {
		t.Fatalf("want ErrInexact, got %v", err)
	}
	if _, err := ConvertExact(1, 0, 48000); !errors.Is(err, ErrZeroDenominator) {
		t.Fatalf("want zero denominator, got %v", err)
	}
}

func TestFromTimestampAndRescale(t *testing.T) {
	ts, err := FromTimestamp(180180, 90000)
	if err != nil {
		t.Fatal(err)
	}
	if ts.String() != "1001/500" {
		t.Fatalf("ts=%s want 1001/500", ts)
	}
	rescaled, err := ts.Rescale(90000, 48000)
	if err != nil {
		t.Fatal(err)
	}
	if rescaled.String() != "2002/1875" {
		t.Fatalf("rescaled=%s", rescaled)
	}
	if _, err := FromTimestamp(1, 0); !errors.Is(err, ErrZeroDenominator) {
		t.Fatalf("zero timescale: %v", err)
	}
	if _, err := FromTimestamp(1<<63, 90000); !errors.Is(err, ErrOverflow) {
		t.Fatalf("overflowing timestamp: %v", err)
	}
}

func TestToIntExact(t *testing.T) {
	if v, err := MustNew(4, 2).ToIntExact(); err != nil || v != 2 {
		t.Fatalf("ToIntExact = %d, %v", v, err)
	}
	if _, err := MustNew(1, 3).ToIntExact(); !errors.Is(err, ErrInexact) {
		t.Fatalf("want ErrInexact, got %v", err)
	}
}

func TestSnap(t *testing.T) {
	unit := MustNew(1, 90000)
	up, err := SnapUp(MustNew(45001, 90000), unit)
	if err != nil {
		t.Fatal(err)
	}
	if !up.Equal(MustNew(45001, 90000)) {
		t.Fatalf("snapup exact multiple changed: %s", up)
	}
	halfPlus, err := MustNew(45000, 90000).Add(MustNew(1, 180000))
	if err != nil {
		t.Fatal(err)
	}
	up, err = SnapUp(halfPlus, unit)
	if err != nil {
		t.Fatal(err)
	}
	if !up.Equal(MustNew(45001, 90000)) {
		t.Fatalf("snapup half+epsilon=%s want 45001/90000", up)
	}
	down, err := SnapDown(MustNew(180001, 180000), unit)
	if err != nil {
		t.Fatal(err)
	}
	if !down.Equal(MustNew(1, 1)) {
		t.Fatalf("snapdown=%s want 1", down)
	}
	if _, err := SnapUp(MustNew(1, 1), MustNew(0, 1)); !errors.Is(err, ErrZeroUnit) {
		t.Fatalf("zero unit: %v", err)
	}
}
