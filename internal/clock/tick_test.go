package clock

import (
	"errors"
	"math"
	"testing"
)

func TestNewNormalizes(t *testing.T) {
	tk, err := New(180000, 90000)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Num != 2 || tk.Den != 1 {
		t.Fatalf("got %v, want 2/1", tk)
	}
	neg, err := New(1, -2)
	if err != nil {
		t.Fatal(err)
	}
	if neg.Num != -1 || neg.Den != 2 {
		t.Fatalf("sign normalization failed: %v", neg)
	}
}

func TestNewRejectsZeroDenominator(t *testing.T) {
	if _, err := New(1, 0); !errors.Is(err, ErrInvalidTimescale) {
		t.Fatalf("want ErrInvalidTimescale, got %v", err)
	}
}

func TestAddSub(t *testing.T) {
	a := Must(1, 2)
	b := Must(1, 3)
	sum, err := a.Add(b)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Num != 5 || sum.Den != 6 {
		t.Fatalf("1/2+1/3 = %v, want 5/6", sum)
	}
	diff, err := a.Sub(b)
	if err != nil {
		t.Fatal(err)
	}
	if diff.Num != 1 || diff.Den != 6 {
		t.Fatalf("1/2-1/3 = %v, want 1/6", diff)
	}
}

func TestAddOverflow(t *testing.T) {
	a := Must(math.MaxInt64, 1)
	if _, err := a.Add(a); !errors.Is(err, ErrOverflow) {
		t.Fatalf("want ErrOverflow, got %v", err)
	}
}

func TestMulKOverflow(t *testing.T) {
	a := Must(math.MaxInt64, 1)
	if _, err := a.MulK(2); !errors.Is(err, ErrOverflow) {
		t.Fatalf("want ErrOverflow, got %v", err)
	}
}

func TestCmpAndSign(t *testing.T) {
	a := Must(1, 3)
	b := Must(2, 6)
	if a.Cmp(b) != 0 {
		t.Fatal("1/3 should equal 2/6")
	}
	if Must(-1, 2).Sign() != -1 || Zero().Sign() != 0 || Must(3, 1).Sign() != 1 {
		t.Fatal("bad sign")
	}
}

func TestMillis(t *testing.T) {
	if got := Must(1, 4).Millis(); got != 250 {
		t.Fatalf("got %v ms, want 250", got)
	}
}
