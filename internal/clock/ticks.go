// Package clock provides an exact rational time base shared by every
// mediapack stage.  A Ticks value is always reduced, has a positive
// denominator, and is guaranteed to fit in int64.  All arithmetic is
// performed with arbitrary precision intermediates so that overflow is
// detected instead of silently wrapping.
package clock

import (
	"errors"
	"fmt"
	"math/big"
)

var (
	// ErrZeroDenominator is returned for a rational time with denominator 0.
	ErrZeroDenominator = errors.New("clock: zero denominator")
	// ErrOverflow is returned when a result cannot be represented by int64.
	ErrOverflow = errors.New("clock: rational overflow")
	// ErrInexact is returned when an exact integer conversion is impossible.
	ErrInexact = errors.New("clock: conversion is not exact")
	// ErrZeroUnit is returned when a snap or division unit is zero.
	ErrZeroUnit = errors.New("clock: zero time unit")
)

// Ticks is an exact rational timestamp.  Num/Den are reduced and Den > 0.
type Ticks struct {
	num int64
	den int64
}

// New builds a reduced Ticks value.  It rejects a zero or overflowing
// denominator and any numerator that cannot be represented in int64.
func New(num, den int64) (Ticks, error) {
	if den == 0 {
		return Ticks{}, ErrZeroDenominator
	}
	n := big.NewInt(num)
	d := big.NewInt(den)
	return reduce(n, d)
}

// MustNew is New for tests and constant expressions.  It panics on error.
func MustNew(num, den int64) Ticks {
	t, err := New(num, den)
	if err != nil {
		panic(err)
	}
	return t
}

func reduce(num, den *big.Int) (Ticks, error) {
	if den.Sign() == 0 {
		return Ticks{}, ErrZeroDenominator
	}
	if den.Sign() < 0 {
		num.Neg(num)
		den.Neg(den)
	}
	abs := new(big.Int).Abs(num)
	g := new(big.Int).GCD(nil, nil, abs, den)
	if g.Sign() != 0 {
		num.Quo(num, g)
		den.Quo(den, g)
	}
	if !num.IsInt64() || !den.IsInt64() {
		return Ticks{}, ErrOverflow
	}
	return Ticks{num: num.Int64(), den: den.Int64()}, nil
}

// norm makes the zero value behave as 0/1.  Go zero values are common in
// accumulators, and a denominator of zero would otherwise poison every
// later comparison.
func (t Ticks) norm() Ticks {
	if t.den == 0 {
		return Ticks{num: t.num, den: 1}
	}
	return t
}

// Num returns the reduced numerator.
func (t Ticks) Num() int64 { return t.norm().num }

// Den returns the reduced, positive denominator.
func (t Ticks) Den() int64 { return t.norm().den }

// IsZero reports whether the value is exactly zero.
func (t Ticks) IsZero() bool { return t.norm().num == 0 }

// Sign returns -1, 0 or +1.
func (t Ticks) Sign() int {
	n := t.norm().num
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

// Neg returns -t.
func (t Ticks) Neg() Ticks {
	t = t.norm()
	return Ticks{num: -t.num, den: t.den}
}

// Abs returns |t|.
func (t Ticks) Abs() Ticks {
	if t.norm().num < 0 {
		return t.Neg()
	}
	return t.norm()
}

// String renders the reduced fraction, for example "3/2".
func (t Ticks) String() string {
	t = t.norm()
	if t.den == 1 {
		return fmt.Sprintf("%d", t.num)
	}
	return fmt.Sprintf("%d/%d", t.num, t.den)
}

// Float64 is a lossy convenience for reporting only; comparisons must use Cmp.
func (t Ticks) Float64() float64 {
	t = t.norm()
	return float64(t.num) / float64(t.den)
}

func (t Ticks) cmp(o Ticks) int {
	// Cross products fit in 128 bits; big.Int keeps this exact and overflow free.
	t, o = t.norm(), o.norm()
	l := new(big.Int).Mul(big.NewInt(t.num), big.NewInt(o.den))
	r := new(big.Int).Mul(big.NewInt(o.num), big.NewInt(t.den))
	return l.Cmp(r)
}

// Cmp compares t and o.
func (t Ticks) Cmp(o Ticks) int { return t.cmp(o) }

// Equal reports exact equality.
func (t Ticks) Equal(o Ticks) bool { return t.cmp(o) == 0 }

// Add returns t + o, rejecting overflow.
func (t Ticks) Add(o Ticks) (Ticks, error) {
	t, o = t.norm(), o.norm()
	n := new(big.Int).Mul(big.NewInt(t.num), big.NewInt(o.den))
	n.Add(n, new(big.Int).Mul(big.NewInt(o.num), big.NewInt(t.den)))
	d := new(big.Int).Mul(big.NewInt(t.den), big.NewInt(o.den))
	return reduce(n, d)
}

// Sub returns t - o, rejecting overflow.
func (t Ticks) Sub(o Ticks) (Ticks, error) {
	return t.Add(o.Neg())
}

// Mul returns t * o, rejecting overflow.
func (t Ticks) Mul(o Ticks) (Ticks, error) {
	t, o = t.norm(), o.norm()
	return reduce(
		new(big.Int).Mul(big.NewInt(t.num), big.NewInt(o.num)),
		new(big.Int).Mul(big.NewInt(t.den), big.NewInt(o.den)),
	)
}

// Div returns t / o, rejecting division by zero and overflow.
func (t Ticks) Div(o Ticks) (Ticks, error) {
	t, o = t.norm(), o.norm()
	if o.IsZero() {
		return Ticks{}, ErrZeroDenominator
	}
	return reduce(
		new(big.Int).Mul(big.NewInt(t.num), big.NewInt(o.den)),
		new(big.Int).Mul(big.NewInt(t.den), big.NewInt(o.num)),
	)
}

// ToIntExact returns the integer value or ErrInexact if t is fractional.
func (t Ticks) ToIntExact() (int64, error) {
	t = t.norm()
	if t.den != 1 {
		return 0, fmt.Errorf("%w: %s", ErrInexact, t)
	}
	return t.num, nil
}

// FromTimestamp converts a media timestamp in units of timescale into ticks.
func FromTimestamp(ts uint64, timescale uint32) (Ticks, error) {
	if timescale == 0 {
		return Ticks{}, ErrZeroDenominator
	}
	if ts > 1<<62 {
		return Ticks{}, ErrOverflow
	}
	return New(int64(ts), int64(timescale))
}

// Rescale converts t expressed in from-units into to-units exactly.  The
// conversion is applied as a rational operation and therefore never rounds.
func (t Ticks) Rescale(from, to uint32) (Ticks, error) {
	if from == 0 {
		return Ticks{}, ErrZeroDenominator
	}
	return t.Mul(MustNew(int64(to), int64(from)))
}

// ConvertExact converts an integer count from one timescale to another and
// fails when the result would not be an integer.  This is used for segment
// boundaries where rounding would make reruns irreproducible.
func ConvertExact(value int64, from, to uint32) (int64, error) {
	if from == 0 {
		return 0, ErrZeroDenominator
	}
	n := new(big.Int).Mul(big.NewInt(value), big.NewInt(int64(to)))
	d := big.NewInt(int64(from))
	rem := new(big.Int).Rem(n, d)
	if rem.Sign() != 0 {
		return 0, fmt.Errorf("%w: %d/%d -> 1/%d", ErrInexact, value, from, to)
	}
	q := new(big.Int).Quo(n, d)
	if !q.IsInt64() {
		return 0, ErrOverflow
	}
	return q.Int64(), nil
}

// SnapUp rounds t up to the next multiple of unit.
func SnapUp(t, unit Ticks) (Ticks, error) {
	if unit.Sign() <= 0 {
		return Ticks{}, ErrZeroUnit
	}
	q, err := t.Div(unit)
	if err != nil {
		return Ticks{}, err
	}
	// Ceiling division for rationals: floor(q); add one unless exact.
	floor := q.num / q.den
	if q.num < 0 && q.num%q.den != 0 {
		floor--
	}
	exact := q.num%q.den == 0
	if !exact {
		floor++
	}
	return unit.Mul(MustNew(floor, 1))
}

// SnapDown rounds t down to the previous multiple of unit.
func SnapDown(t, unit Ticks) (Ticks, error) {
	if unit.Sign() <= 0 {
		return Ticks{}, ErrZeroUnit
	}
	q, err := t.Div(unit)
	if err != nil {
		return Ticks{}, err
	}
	floor := q.num / q.den
	if q.num < 0 && q.num%q.den != 0 {
		floor--
	}
	return unit.Mul(MustNew(floor, 1))
}
