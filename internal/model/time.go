// Package model defines the time base and the unified error model shared by
// every archive subsystem. All time arithmetic stays in integer rationals so
// that no precision is lost across tracks with different time scales.
package model

import (
	"fmt"
	"math/big"
)

// Rational is an exact signed rational number. Num may be negative; Den is
// always positive. The zero value is 0/1.
type Rational struct {
	Num int64
	Den int64
}

// NewRational builds a normalized rational. A zero numerator with a zero
// denominator is the package zero value and normalizes to 0/1; a non-zero
// numerator with a zero denominator is a genuine error and panics.
func NewRational(num, den int64) Rational {
	if den == 0 {
		if num == 0 {
			return Rational{Num: 0, Den: 1}
		}
		panic("rational with zero denominator")
	}
	if den < 0 {
		num, den = -num, -den
	}
	if num == 0 {
		return Rational{Num: 0, Den: 1}
	}
	g := new(big.Int).GCD(nil, nil, big.NewInt(abs64(num)), big.NewInt(den))
	return Rational{Num: num / g.Int64(), Den: den / g.Int64()}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// norm guarantees a positive denominator so the package zero value
// Rational{} behaves as 0/1 in every operation.
func (r Rational) norm() Rational {
	if r.Den == 0 {
		return Rational{Num: 0, Den: 1}
	}
	return r
}

// Add returns r+s exactly.
func (r Rational) Add(s Rational) Rational {
	r, s = r.norm(), s.norm()
	return NewRational(r.Num*s.Den+s.Num*r.Den, r.Den*s.Den)
}

// Sub returns r-s exactly.
func (r Rational) Sub(s Rational) Rational {
	r, s = r.norm(), s.norm()
	return NewRational(r.Num*s.Den-s.Num*r.Den, r.Den*s.Den)
}

// Mul returns r*s exactly.
func (r Rational) Mul(s Rational) Rational {
	r, s = r.norm(), s.norm()
	return NewRational(r.Num*s.Num, r.Den*s.Den)
}

// Div returns r/s exactly and panics on division by zero.
func (r Rational) Div(s Rational) Rational {
	r, s = r.norm(), s.norm()
	return NewRational(r.Num*s.Den, r.Den*s.Num)
}

// Float64 converts to a floating point value for reporting only.
func (r Rational) Float64() float64 {
	r = r.norm()
	if r.Den == 0 {
		return 0
	}
	return float64(r.Num) / float64(r.Den)
}

// Nanoseconds returns the value truncated to whole nanoseconds.
func (r Rational) Nanoseconds() int64 {
	r = r.norm()
	return r.Num * 1_000_000_000 / r.Den
}

// Cmp returns -1, 0 or 1 like bytes.Compare.
func (r Rational) Cmp(s Rational) int {
	r, s = r.norm(), s.norm()
	l := big.NewInt(r.Num)
	l.Mul(l, big.NewInt(s.Den))
	rr := big.NewInt(s.Num)
	rr.Mul(rr, big.NewInt(r.Den))
	return l.Cmp(rr)
}

func (r Rational) String() string {
	if r.Den == 1 {
		return fmt.Sprintf("%d", r.Num)
	}
	return fmt.Sprintf("%d/%d", r.Num, r.Den)
}

// IsZero reports whether the value equals zero.
func (r Rational) IsZero() bool { return r.Num == 0 }
