// Package model defines the shared rational time base and the structured
// error model used by every stage of the archive-restoration pipeline.
//
// All time-base conversions are exact rational (integer) operations:
// cross-sample-rate PTS arithmetic must never accumulate floating-point
// drift, because missing blocks and truncation make fragment boundaries
// fragile and the pipeline has to prove monotonic PTS invariants.
package model

import (
	"errors"
	"fmt"
)

// SampleRate is a PCM clock rate in hertz. Supported rates span 8–192 kHz.
type SampleRate uint32

const (
	// MinSampleRate / MaxSampleRate bound the accepted PCM clock rates.
	MinSampleRate SampleRate = 8000
	MaxSampleRate SampleRate = 192000
)

// Valid reports whether the rate is inside the supported 8–192 kHz range.
func (sr SampleRate) Valid() bool {
	return sr >= MinSampleRate && sr <= MaxSampleRate
}

// Rational is an exact rational number num/den with den > 0, always stored
// in lowest terms. The zero value is 0/1 and is safe to use.
type Rational struct {
	num int64
	den int64
}

// NewRational builds num/den, rejecting a non-positive denominator, and
// reduces the fraction to lowest terms.
func NewRational(num, den int64) (Rational, error) {
	if den <= 0 {
		return Rational{}, fmt.Errorf("rational denominator must be positive, got %d", den)
	}
	if num == 0 {
		return Rational{num: 0, den: 1}, nil
	}
	g := gcd(abs64(num), den)
	return Rational{num: num / g, den: den / g}, nil
}

// MustRational panics instead of returning an error; use only for literals
// known at compile time.
func MustRational(num, den int64) Rational {
	r, err := NewRational(num, den)
	if err != nil {
		panic(err)
	}
	return r
}

// Num returns the numerator.
func (r Rational) Num() int64 { return r.num }

// Den returns the (positive) denominator.
func (r Rational) Den() int64 {
	if r.den == 0 {
		return 1
	}
	return r.den
}

// IsZero reports whether the value equals zero.
func (r Rational) IsZero() bool { return r.num == 0 }

// Add returns r+o exactly.
func (r Rational) Add(o Rational) Rational {
	a, b := r.pair(), o.pair()
	return reduce(a.num*b.den+b.num*a.den, a.den*b.den)
}

// Sub returns r-o exactly.
func (r Rational) Sub(o Rational) Rational {
	a, b := r.pair(), o.pair()
	return reduce(a.num*b.den-b.num*a.den, a.den*b.den)
}

// Mul returns r*o exactly, cross-cancelling factors first to stay inside
// int64 for realistic sample counts.
func (r Rational) Mul(o Rational) Rational {
	a, b := r.pair(), o.pair()
	g1 := gcd(abs64(a.num), b.den)
	g2 := gcd(abs64(b.num), a.den)
	return Rational{num: (a.num / g1) * (b.num / g2), den: (a.den / g2) * (b.den / g1)}
}

// Cmp returns -1, 0 or 1 as r is less than, equal to, or greater than o.
func (r Rational) Cmp(o Rational) int {
	a, b := r.pair(), o.pair()
	l := a.num * b.den
	rr := b.num * a.den
	switch {
	case l < rr:
		return -1
	case l > rr:
		return 1
	default:
		return 0
	}
}

// Neg returns -r.
func (r Rational) Neg() Rational { return Rational{num: -r.num, den: r.Den()} }

// Abs returns |r|.
func (r Rational) Abs() Rational {
	if r.num < 0 {
		return r.Neg()
	}
	return r
}

// Microseconds rounds the value to the nearest whole microsecond
// (half away from zero). It exists for reporting only; pipeline math
// keeps using the exact value.
func (r Rational) Microseconds() int64 {
	num := r.num * 1_000_000
	if num >= 0 {
		return (num + r.Den()/2) / r.Den()
	}
	return -((-num + r.Den()/2) / r.Den())
}

// String renders num/den (or "0" for the zero value).
func (r Rational) String() string {
	if r.num == 0 {
		return "0"
	}
	return fmt.Sprintf("%d/%d", r.num, r.Den())
}

func (r Rational) pair() Rational {
	if r.den == 0 {
		return Rational{num: 0, den: 1}
	}
	return r
}

func reduce(num, den int64) Rational {
	if den == 0 {
		return Rational{}
	}
	if num == 0 {
		return Rational{num: 0, den: 1}
	}
	g := gcd(abs64(num), den)
	return Rational{num: num / g, den: den / g}
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// SampleToPTS converts an integer sample position on the clock hz into an
// exact presentation timestamp in seconds.
func SampleToPTS(pos int64, hz SampleRate) Rational {
	r, err := NewRational(pos, int64(hz))
	if err != nil {
		return Rational{}
	}
	return r
}

// ConvertSample maps a sample position expressed on clock src onto the
// nearest equivalent sample position on clock dst using integer math only
// (half away from zero). No floating-point state is ever accumulated.
func ConvertSample(pos int64, src, dst SampleRate) (int64, error) {
	if !src.Valid() || !dst.Valid() {
		return 0, fmt.Errorf("invalid sample rate in conversion: src=%d dst=%d", src, dst)
	}
	num := pos * int64(dst)
	den := int64(src)
	if num >= 0 {
		return (num + den/2) / den, nil
	}
	return -((-num + den/2) / den), nil
}

// ErrInvalidTime is returned when a time-base invariant is violated.
var ErrInvalidTime = errors.New("invalid time-base value")
