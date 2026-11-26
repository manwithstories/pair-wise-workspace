// Package clock provides the single rational time basis used by container
// metadata, RTP recovery, A/V sync and verification.
package clock

import "fmt"

// Rational is an exact signed fraction. Denominators are always positive
// after construction through New or Rat.
type Rational struct {
	N int64
	D int64
}

// Rat returns a normalized rational. A zero denominator is rejected.
func Rat(n, d int64) (Rational, error) {
	if d == 0 {
		return Rational{}, fmt.Errorf("clock: zero denominator")
	}
	if d < 0 {
		n, d = -n, -d
	}
	g := gcd(abs(n), d)
	return Rational{N: n / g, D: d / g}, nil
}

// MustRat is intended for compile-time-known test and fixture values.
func MustRat(n, d int64) Rational {
	r, err := Rat(n, d)
	if err != nil {
		panic(err)
	}
	return r
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		return 1
	}
	return a
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// Cmp compares two rationals without floating point.
func Cmp(a, b Rational) int {
	an := a.N * b.D
	bn := b.N * a.D
	switch {
	case an < bn:
		return -1
	case an > bn:
		return 1
	default:
		return 0
	}
}

// Mul returns a*b in normalized form.
func Mul(a, b Rational) Rational {
	r, _ := Rat(a.N*b.N, a.D*b.D)
	return r
}

// Float64 is only for human-readable output; decisions use Cmp/Milliseconds.
func (r Rational) Float64() float64 {
	return float64(r.N) / float64(r.D)
}

// Milliseconds rounds the rational value to the nearest millisecond.
func (r Rational) Milliseconds() int64 {
	if r.D == 0 {
		return 0
	}
	num := r.N * 1000
	q := num / r.D
	rem := num % r.D
	if rem < 0 && -2*rem >= r.D {
		q--
	} else if rem > 0 && 2*rem >= r.D {
		q++
	}
	return q
}

// Time is a tick count on a rational time base. The represented time in
// seconds is Ticks * Base.N / Base.D.
type Time struct {
	Ticks int64
	Base  Rational
}

// NewTime builds a time on a ticks-per-second timescale.
func NewTime(ticks int64, timescale uint32) Time {
	if timescale == 0 {
		timescale = 1
	}
	return Time{Ticks: ticks, Base: Rational{N: 1, D: int64(timescale)}}
}

// FromMilliseconds creates a time on the canonical 1/1000-second basis.
func FromMilliseconds(ms int64) Time {
	return Time{Ticks: ms, Base: Rational{N: 1, D: 1000}}
}

// Rational returns the exact represented time in seconds.
func (t Time) Rational() Rational {
	return Mul(Rational{N: t.Ticks, D: 1}, t.Base)
}

// Milliseconds returns the nearest millisecond on the unified timeline.
func (t Time) Milliseconds() int64 {
	return t.Rational().Milliseconds()
}

// DeltaUint32 returns the signed shortest forward difference b-a after a
// 32-bit timestamp wrap. Inputs are modulo 2^32.
func DeltaUint32(a, b uint32) int64 {
	return int64(int32(b - a))
}

// DeltaUint16 returns the signed shortest forward difference b-a after a
// 16-bit sequence-number wrap.
func DeltaUint16(a, b uint16) int {
	return int(int16(b - a))
}
