// Package model defines the unified rational time base, timeline types and
// error taxonomy shared by every pipeline stage.
package model

import (
	"errors"
	"fmt"
)

// Rat is a checked rational number with int64 numerator and denominator.
// Denominator is always positive; the zero value is 0/1. All arithmetic
// returns ErrOverflow instead of silently wrapping.
type Rat struct {
	num int64
	den int64
}

// NewRat constructs a rational and rejects a non-positive denominator.
func NewRat(num, den int64) (Rat, error) {
	if den <= 0 {
		return Rat{}, ErrOverflow
	}
	if num == 0 {
		return Rat{0, 1}, nil
	}
	g := gcd(abs64(num), den)
	return Rat{num / g, den / g}, nil
}

// MustRat panics on invalid input; intended for package-level constants.
func MustRat(num, den int64) Rat {
	r, err := NewRat(num, den)
	if err != nil {
		panic(err)
	}
	return r
}

// Num returns the numerator.
func (r Rat) Num() int64 { return r.num }

// Den returns the denominator (always >= 1).
func (r Rat) Den() int64 {
	if r.den == 0 {
		return 1
	}
	return r.den
}

// IsZero reports whether the value equals zero.
func (r Rat) IsZero() bool { return r.num == 0 }

// Neg flips the sign.
func (r Rat) Neg() Rat { return Rat{-r.num, r.Den()} }

// Add performs checked rational addition.
func (r Rat) Add(o Rat) (Rat, error) {
	d1, d2 := r.Den(), o.Den()
	g := gcd(d1, d2)
	lcm, err := mul64(d1/g, d2)
	if err != nil {
		return Rat{}, err
	}
	n1, err := mul64(r.num, lcm/d1)
	if err != nil {
		return Rat{}, err
	}
	n2, err := mul64(o.num, lcm/d2)
	if err != nil {
		return Rat{}, err
	}
	n, err := add64(n1, n2)
	if err != nil {
		return Rat{}, err
	}
	return NewRat(n, lcm)
}

// Sub performs checked rational subtraction.
func (r Rat) Sub(o Rat) (Rat, error) {
	return r.Add(o.Neg())
}

// Mul performs checked rational multiplication.
func (r Rat) Mul(o Rat) (Rat, error) {
	n, err := mul64(r.num, o.num)
	if err != nil {
		return Rat{}, err
	}
	d, err := mul64(r.Den(), o.Den())
	if err != nil {
		return Rat{}, err
	}
	return NewRat(n, d)
}

// Cmp returns -1, 0 or 1.
func (r Rat) Cmp(o Rat) int {
	ln, err := mul64(r.num, o.Den())
	if err != nil {
		panic(ErrOverflow)
	}
	rn, err := mul64(o.num, r.Den())
	if err != nil {
		panic(ErrOverflow)
	}
	switch {
	case ln < rn:
		return -1
	case ln > rn:
		return 1
	default:
		return 0
	}
}

// Millis rounds the value, expressed in seconds, to whole milliseconds.
func (r Rat) Millis() (int64, error) {
	ms, err := r.Mul(MustRat(1000, 1))
	if err != nil {
		return 0, err
	}
	return ms.num / ms.Den(), nil
}

// String renders the canonical "num/den" form.
func (r Rat) String() string { return fmt.Sprintf("%d/%d", r.num, r.Den()) }

// Seconds builds a rational duration in seconds from timescale units.
func Seconds(units, timescale int64) (Rat, error) {
	if timescale <= 0 {
		return Rat{}, ErrOverflow
	}
	return NewRat(units, timescale)
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func abs64(v int64) int64 {
	if v < 0 {
		if v == -1<<63 {
			return 1<<63 - 1
		}
		return -v
	}
	return v
}

func mul64(a, b int64) (int64, error) {
	r := a * b
	if a != 0 && r/a != b {
		return 0, ErrOverflow
	}
	return r, nil
}

func add64(a, b int64) (int64, error) {
	r := a + b
	if (a^r)&(b^r) < 0 {
		return 0, ErrOverflow
	}
	return r, nil
}

// ErrOverflow is returned when checked arithmetic overflows int64.
var ErrOverflow = errors.New("rational arithmetic overflow")
