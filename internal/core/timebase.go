// Package core defines the rational time base, safe path handling and the
// shared fault model used by every subsystem of the media validator.
package core

import (
	"fmt"
	"math/big"
)

// Rational is a signed rational number. Durations inside the tool are always
// expressed as a numerator over an explicit denominator (a time scale), so
// that DASH timescale units, HLS floating point seconds and RTP clock rates
// can be compared without accumulating floating point error.
type Rational struct {
	Num int64
	Den int64 // always strictly positive
}

// NewRational builds a normalized rational, panicking on a non-positive
// denominator. Programmers supply constants/parsed scales here; bad input is
// rejected at the parser boundary instead.
func NewRational(num, den int64) Rational {
	if den <= 0 {
		panic(fmt.Sprintf("core: non-positive denominator %d", den))
	}
	r := Rational{Num: num, Den: den}
	return r.normalize()
}

func (r Rational) normalize() Rational {
	if r.Num == 0 {
		return Rational{Num: 0, Den: r.Den}
	}
	g := new(big.Int).GCD(nil, nil, big.NewInt(abs64(r.Num)), big.NewInt(r.Den)).Int64()
	return Rational{Num: r.Num / g, Den: r.Den / g}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// Int64 returns the value rounded toward zero.
func (r Rational) Int64() int64 { return r.Num / r.Den }

// Float64 returns the value as floating point, used for terminal reports only.
func (r Rational) Float64() float64 { return float64(r.Num) / float64(r.Den) }

// Scale converts a value expressed in units of src scale into dst scale,
// rounding to the nearest integer (half away from zero).
func Scale(value, srcScale, dstScale int64) int64 {
	if srcScale <= 0 || dstScale <= 0 {
		panic("core: scale must be positive")
	}
	num := value * dstScale
	if num == 0 {
		return 0
	}
	if num > 0 {
		return (num + srcScale/2) / srcScale
	}
	return -((-num + srcScale/2) / srcScale)
}

// ToScale converts r into an integer tick count at the given time scale.
func (r Rational) ToScale(scale int64) int64 {
	return Scale(r.Num, r.Den, scale)
}

// Millis returns the duration in milliseconds, rounded.
func (r Rational) Millis() int64 { return r.ToScale(1000) }

// SecondsR builds a rational number of seconds.
func SecondsR(seconds int64) Rational { return Rational{Num: seconds, Den: 1} }

// MillisR builds a rational from a millisecond count.
func MillisR(ms int64) Rational { return Rational{Num: ms, Den: 1000} }

// TicksR builds a rational from ticks of an explicit time scale.
func TicksR(ticks, scale int64) Rational { return NewRational(ticks, scale) }

// Add returns r+o in normalized form.
func (r Rational) Add(o Rational) Rational {
	return NewRational(r.Num*o.Den+o.Num*r.Den, r.Den*o.Den)
}

// Sub returns r-o in normalized form.
func (r Rational) Sub(o Rational) Rational {
	return NewRational(r.Num*o.Den-o.Num*r.Den, r.Den*o.Den)
}

// Mul scales r by an integer factor.
func (r Rational) Mul(n int64) Rational { return NewRational(r.Num*n, r.Den) }

// Cmp returns -1, 0, 1 like bytes.Compare.
func (r Rational) Cmp(o Rational) int {
	l := r.Num * o.Den
	right := o.Num * r.Den
	switch {
	case l < right:
		return -1
	case l > right:
		return 1
	default:
		return 0
	}
}

// IsNegative reports whether the value is below zero.
func (r Rational) IsNegative() bool { return r.Num < 0 }

// String renders the canonical "n/d" form, or "n" for unit denominators.
func (r Rational) String() string {
	if r.Den == 1 {
		return fmt.Sprintf("%d", r.Num)
	}
	return fmt.Sprintf("%d/%d", r.Num, r.Den)
}
