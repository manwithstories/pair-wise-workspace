package timeline

import (
	"math"
	"testing"

	"mediadeliver/internal/model"
)

func TestNewRatRejectsBadDenominator(t *testing.T) {
	for _, den := range []int64{0, -1} {
		if _, err := NewRat(1, den); model.KindOf(err) != model.KindInvalidInput {
			t.Fatalf("den=%d: want KindInvalidInput, got %v", den, err)
		}
	}
}

func TestRatAddSubCmp(t *testing.T) {
	a := MustNewRat(1, 3)
	b := MustNewRat(1, 6)
	sum, err := a.Add(b)
	if err != nil || sum.String() != "1/2" {
		t.Fatalf("add: %s err=%v", sum, err)
	}
	diff, err := a.Sub(b)
	if err != nil || diff.String() != "1/6" {
		t.Fatalf("sub: %s err=%v", diff, err)
	}
	if a.Cmp(b) <= 0 || b.Cmp(a) >= 0 || a.Cmp(a) != 0 {
		t.Fatal("comparison wrong")
	}
	if !MustNewRat(-1, 2).IsNegative() || a.IsNegative() {
		t.Fatal("sign check wrong")
	}
}

func TestScaleToFloor(t *testing.T) {
	r := MustNewRat(7, 2)
	v, err := r.ScaleTo(1000) // 3500
	if err != nil || v != 3500 {
		t.Fatalf("scale exact: %d %v", v, err)
	}
	v, err = MustNewRat(1, 3).ScaleTo(10) // floor(3.333)
	if err != nil || v != 3 {
		t.Fatalf("scale floor: %d %v", v, err)
	}
	v, err = MustNewRat(-1, 3).ScaleTo(10) // floor(-3.333) = -4
	if err != nil || v != -4 {
		t.Fatalf("scale negative floor: %d %v", v, err)
	}
	if _, err := r.ScaleTo(0); model.KindOf(err) != model.KindInvalidInput {
		t.Fatalf("bad dst den: %v", err)
	}
}

func TestOverflowArithmetic(t *testing.T) {
	big := MustNewRat(math.MaxInt64, 1)
	one := MustNewRat(1, 1)
	if _, err := big.Add(one); model.KindOf(err) != model.KindOverflow {
		t.Fatalf("add overflow: %v", err)
	}
	if _, err := big.Mul(MustNewRat(2, 1)); err == nil {
		t.Fatal("expected mul overflow")
	}
	if _, err := MustNewRat(math.MinInt64, 1).Sub(one); model.KindOf(err) != model.KindOverflow {
		t.Fatalf("sub overflow: %v", err)
	}
}

func TestCheckNonNegative(t *testing.T) {
	if err := CheckNonNegative(MustNewRat(0, 1), "dur"); err != nil {
		t.Fatal(err)
	}
	if err := CheckNonNegative(MustNewRat(-5, 2), "dur"); model.KindOf(err) != model.KindInvalidInput {
		t.Fatalf("want invalid input, got %v", err)
	}
}
