// Package core provides the shared rational-second timebase and the
// error-range type used by every subsystem (manifest, index, segment,
// pipeline, provenance).
package core

import "strconv"

// Rat is an exact rational number of seconds, always kept normalized:
// Den > 0 and gcd(|Num|, Den) == 1. The zero value is 0/1.
type Rat struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

func gcd(a, b int64) int64 {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		return 1
	}
	return a
}

// NewRat returns the normalized form of num/den. A zero denominator is
// treated as 1 so callers can never construct an invalid Rat.
func NewRat(num, den int64) Rat {
	if den == 0 {
		den = 1
	}
	if den < 0 {
		num, den = -num, -den
	}
	if num == 0 {
		return Rat{0, 1}
	}
	g := gcd(num, den)
	return Rat{num / g, den / g}
}

// RatFromUnits converts value/timescale (e.g. DASH media time) to seconds.
func RatFromUnits(value, timescale int64) Rat { return NewRat(value, timescale) }

// RatFromMillis converts milliseconds to seconds.
func RatFromMillis(ms int64) Rat { return NewRat(ms, 1000) }

// norm repairs a zero-value Rat (Den == 0) so the zero value behaves
// as 0/1 in every operation.
func (r Rat) norm() Rat {
	if r.Den <= 0 {
		if r.Den < 0 {
			r.Num = -r.Num
			r.Den = -r.Den
		} else {
			r.Den = 1
		}
	}
	return r
}

func (r Rat) Add(o Rat) Rat {
	r, o = r.norm(), o.norm()
	return NewRat(r.Num*o.Den+o.Num*r.Den, r.Den*o.Den)
}

func (r Rat) Sub(o Rat) Rat {
	r, o = r.norm(), o.norm()
	return NewRat(r.Num*o.Den-o.Num*r.Den, r.Den*o.Den)
}

// Mul scales r by an integer factor.
func (r Rat) Mul(n int64) Rat {
	r = r.norm()
	return NewRat(r.Num*n, r.Den)
}

func (r Rat) Cmp(o Rat) int {
	r, o = r.norm(), o.norm()
	l, rr := r.Num*o.Den, o.Num*r.Den
	switch {
	case l < rr:
		return -1
	case l > rr:
		return 1
	}
	return 0
}

func (r Rat) IsZero() bool     { return r.Num == 0 }
func (r Rat) IsNegative() bool { return r.Num < 0 }

func (r Rat) Float64() float64 {
	r = r.norm()
	return float64(r.Num) / float64(r.Den)
}

// String renders the canonical "num/den" form ("3" for 3/1). It is used in
// reports, so it must stay deterministic.
func (r Rat) String() string {
	r = r.norm()
	if r.Den == 1 {
		return strconv.FormatInt(r.Num, 10)
	}
	return strconv.FormatInt(r.Num, 10) + "/" + strconv.FormatInt(r.Den, 10)
}
