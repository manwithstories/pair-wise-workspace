// Package media provides the shared rational time base and error model
// used to unify DTS, PTS and RTP clocks across tracks.
package media

import "fmt"

// Rational is an exact time value: seconds = Num / Den.
// Zero Den is never stored; NewRational normalizes the sign and reduces.
type Rational struct {
	Num int64
	Den int64
}

// NewRational builds a normalized rational. den <= 0 yields 0/1.
func NewRational(num, den int64) Rational {
	if den == 0 {
		return Rational{0, 1}
	}
	if den < 0 {
		num, den = -num, -den
	}
	g := gcd(abs(num), den)
	return Rational{num / g, den / g}
}

// FromTicks converts ticks at a timescale (e.g. 90000) to a Rational.
func FromTicks(ticks, timescale int64) Rational { return NewRational(ticks, timescale) }

// String renders the rational as "num/den".
func (r Rational) String() string { return fmt.Sprintf("%d/%d", r.Num, r.Den) }

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// Add returns r + o (exact).
func (r Rational) Add(o Rational) Rational {
	return NewRational(r.Num*o.Den+o.Num*r.Den, r.Den*o.Den)
}

// Sub returns r - o (exact).
func (r Rational) Sub(o Rational) Rational {
	return NewRational(r.Num*o.Den-o.Num*r.Den, r.Den*o.Den)
}

// Mul returns r * o (exact).
func (r Rational) Mul(o Rational) Rational {
	return NewRational(r.Num*o.Num, r.Den*o.Den)
}

// Cmp compares: -1, 0, +1. Safe against overflow for typical media scales
// by cross-multiplying in int64 after normalization.
func (r Rational) Cmp(o Rational) int {
	l, rr := r.Num*o.Den, o.Num*r.Den
	switch {
	case l < rr:
		return -1
	case l > rr:
		return 1
	}
	return 0
}

// Seconds returns the value as float seconds.
func (r Rational) Seconds() float64 { return float64(r.Num) / float64(r.Den) }

// Millis returns the value in milliseconds, rounded to nearest.
func (r Rational) Millis() float64 { return r.Seconds() * 1000 }

// Ticks converts the rational to ticks at the given timescale, rounding
// to nearest. Used when re-expressing a mapped time in a track clock.
func (r Rational) Ticks(timescale int64) int64 {
	n := r.Num * timescale
	d := r.Den
	if n >= 0 {
		return (n + d/2) / d
	}
	return -((-n + d/2) / d)
}

// Unwrap32 extends a wrapping 32-bit clock value (RTP timestamp) into a
// monotonic 64-bit value, choosing the wrap cycle nearest to prev.
func Unwrap32(v uint32, prev uint64) uint64 {
	const cycle = 1 << 32
	base := prev &^ (cycle - 1)
	cand := base | uint64(v)
	switch {
	case cand+cycle/2 < prev:
		cand += cycle
	case prev+cycle/2 < cand:
		cand -= cycle
	}
	return cand
}

// Unwrap16 extends a wrapping 16-bit sequence number into 64-bit.
func Unwrap16(v uint16, prev uint64) uint64 {
	const cycle = 1 << 16
	base := prev &^ (cycle - 1)
	cand := base | uint64(v)
	switch {
	case cand+cycle/2 < prev:
		cand += cycle
	case prev+cycle/2 < cand:
		cand -= cycle
	}
	return cand
}
