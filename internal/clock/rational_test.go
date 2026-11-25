package clock

import (
	"errors"
	"math"
	"testing"
)

func TestNewReducesAndNormalizes(t *testing.T) {
	r, err := New(2, 4)
	if err != nil || r != (Ratio{1, 2}) {
		t.Fatalf("New(2,4) = %v, %v", r, err)
	}
	r, err = New(1, -2)
	if err != nil || r != (Ratio{-1, 2}) {
		t.Fatalf("New(1,-2) = %v, %v", r, err)
	}
	r, err = New(-3, -9)
	if err != nil || r != (Ratio{1, 3}) {
		t.Fatalf("New(-3,-9) = %v, %v", r, err)
	}
}

func TestZeroDenominatorRejected(t *testing.T) {
	if _, err := New(1, 0); !errors.Is(err, ErrZeroDenominator) {
		t.Fatalf("expected ErrZeroDenominator, got %v", err)
	}
}

func TestExactArithmetic(t *testing.T) {
	a, _ := New(1, 3)
	b, _ := New(1, 6)
	sum, err := a.Add(b)
	if err != nil || sum != (Ratio{1, 2}) {
		t.Fatalf("1/3+1/6 = %v, %v", sum, err)
	}
	three, _ := New(3, 1)
	one, err := a.Mul(three)
	if err != nil || one != (Ratio{1, 1}) {
		t.Fatalf("1/3*3 = %v, %v", one, err)
	}
	diff, err := a.Sub(b)
	if err != nil || diff != (Ratio{1, 6}) {
		t.Fatalf("1/3-1/6 = %v, %v", diff, err)
	}
}

func TestOverflowRejected(t *testing.T) {
	big := Ratio{math.MaxInt64, 1}
	one := Ratio{1, 1}
	if _, err := big.Add(one); !errors.Is(err, ErrOverflow) {
		t.Fatalf("add: expected ErrOverflow, got %v", err)
	}
	if _, err := big.Mul(Ratio{2, 1}); !errors.Is(err, ErrOverflow) {
		t.Fatalf("mul: expected ErrOverflow, got %v", err)
	}
	if _, err := big.Sub(Ratio{-1, 1}); !errors.Is(err, ErrOverflow) {
		t.Fatalf("sub: expected ErrOverflow, got %v", err)
	}
}

func TestCmpSignAndMillis(t *testing.T) {
	a, _ := New(1, 2)
	b, _ := New(2, 3)
	if a.Cmp(b) != -1 || b.Cmp(a) != 1 || a.Cmp(a) != 0 {
		t.Fatal("Cmp inconsistent")
	}
	neg, _ := New(-1, 2)
	if neg.Sign() != -1 || a.Sign() != 1 || Zero().Sign() != 0 {
		t.Fatal("Sign inconsistent")
	}
	if got := a.Millis(); got != 500 {
		t.Fatalf("1/2 in ms = %v", got)
	}
}

func TestTicks(t *testing.T) {
	r, err := Ticks(48000, 48000)
	if err != nil || r != (Ratio{1, 1}) {
		t.Fatalf("Ticks(48000,48000) = %v, %v", r, err)
	}
	if _, err := Ticks(1, 0); err == nil {
		t.Fatal("expected error for zero timescale")
	}
	if _, err := Ticks(1, -5); err == nil {
		t.Fatal("expected error for negative timescale")
	}
}

func TestMinMax(t *testing.T) {
	a, _ := New(1, 3)
	b, _ := New(1, 4)
	if Min(a, b) != b || Max(a, b) != a {
		t.Fatal("Min/Max inconsistent")
	}
}
