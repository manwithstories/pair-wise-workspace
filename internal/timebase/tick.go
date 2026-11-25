// Package timebase provides exact rational media-time conversion.
package timebase

import (
	"errors"
	"math/big"

	"bcast/internal/fault"
)

// Rational is a normalized signed rational. D is always positive after Normalize.
type Rational struct {
	N int64 `json:"n"`
	D int64 `json:"d"`
}

// New returns a normalized rational and rejects a zero denominator.
func New(n, d int64) (Rational, error) {
	if d == 0 {
		return Rational{}, fault.E(fault.TimelineInvalid, "timebase", "New", errors.New("zero denominator"))
	}
	if d < 0 {
		n, d = -n, -d
	}
	g := gcd(abs(n), d)
	return Rational{N: n / g, D: d / g}, nil
}

// MustNew is for compile-time-known valid ratios.
func MustNew(n, d int64) Rational {
	r, err := New(n, d)
	if err != nil {
		panic(err)
	}
	return r
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
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

func bigRatio(r Rational) *big.Rat {
	return new(big.Rat).SetFrac(big.NewInt(r.N), big.NewInt(r.D))
}

// Normalize reduces the rational and fixes denominator sign.
func (r Rational) Normalize() Rational {
	n, d := r.N, r.D
	if d == 0 {
		return Rational{N: 0, D: 1}
	}
	if d < 0 {
		n, d = -n, -d
	}
	g := gcd(abs(n), d)
	return Rational{N: n / g, D: d / g}
}

// Cmp compares two rationals exactly.
func Cmp(a, b Rational) int {
	return bigRatio(a.Normalize()).Cmp(bigRatio(b.Normalize()))
}

// Add returns a+b without floating-point rounding.
func Add(a, b Rational) Rational {
	x := bigRatio(a)
	x.Add(x, bigRatio(b))
	return ratToR(x)
}

// Sub returns a-b.
func Sub(a, b Rational) Rational {
	x := bigRatio(a)
	x.Sub(x, bigRatio(b))
	return ratToR(x)
}

// Mul returns a*b.
func Mul(a, b Rational) Rational {
	x := bigRatio(a)
	x.Mul(x, bigRatio(b))
	return ratToR(x)
}

// Div returns a/b.
func Div(a, b Rational) (Rational, error) {
	if b.N == 0 {
		return Rational{}, fault.E(fault.TimelineInvalid, "timebase", "Div", errors.New("division by zero"))
	}
	x := bigRatio(a)
	x.Quo(x, bigRatio(b))
	return ratToR(x), nil
}

func ratToR(x *big.Rat) Rational {
	return Rational{N: x.Num().Int64(), D: x.Denom().Int64()}
}

// SampleTime converts a sample count in a timescale (ticks/second) to seconds.
func SampleTime(ticks uint64, timescale uint32) Rational {
	if timescale == 0 {
		return Rational{}
	}
	r, _ := New(int64(ticks), int64(timescale))
	return r
}

// Rescale converts a tick value from one ticks-per-second rate to another.
// Rounding is nearest with half-up; callers needing frame boundaries should use
// FrameBoundary instead.
func Rescale(ticks int64, fromRate, toRate uint32) int64 {
	if fromRate == 0 || toRate == 0 {
		return 0
	}
	num := new(big.Int).Mul(big.NewInt(ticks), big.NewInt(int64(toRate)))
	den := big.NewInt(int64(fromRate))
	num.Add(num, new(big.Int).Quo(den, big.NewInt(2)))
	return new(big.Int).Quo(num, den).Int64()
}

// FrameBoundary rounds t to the nearest frame duration and returns frame index.
func FrameBoundary(t Rational, frameDuration Rational) (int64, error) {
	if frameDuration.N <= 0 || frameDuration.D <= 0 {
		return 0, fault.E(fault.TimelineInvalid, "timebase", "FrameBoundary", errors.New("invalid frame duration"))
	}
	// t / frameDuration = t.N*frameDuration.D / (t.D*frameDuration.N)
	num := new(big.Int).Mul(big.NewInt(t.N), big.NewInt(frameDuration.D))
	den := new(big.Int).Mul(big.NewInt(t.D), big.NewInt(frameDuration.N))
	if den.Sign() == 0 {
		return 0, fault.E(fault.TimelineInvalid, "timebase", "FrameBoundary", errors.New("zero rational denominator"))
	}
	half := new(big.Int).Quo(den, big.NewInt(2))
	if num.Sign() >= 0 {
		num.Add(num, half)
	} else {
		num.Sub(num, half)
	}
	q := new(big.Int).Quo(num, den)
	return q.Int64(), nil
}

// FrameTime returns the exact time of frame index i at frameDuration.
func FrameTime(i int64, frameDuration Rational) Rational {
	return Mul(Rational{N: i, D: 1}, frameDuration)
}
