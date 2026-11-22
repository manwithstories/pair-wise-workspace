// Package clock provides an exact rational time base and wrap-safe
// counters shared by demux, track mapping and remux.
package clock

import (
	"errors"
	"fmt"
	"math/big"
)

// ErrOverflow is returned when rational arithmetic would overflow int64.
var ErrOverflow = errors.New("clock: rational arithmetic overflow")

// Rational is an exact number of seconds, Num/Den, with Den > 0.
type Rational struct {
	Num int64
	Den int64
}

// New returns num/den normalized; den must be positive.
func New(num, den int64) (Rational, error) {
	if den <= 0 {
		return Rational{}, fmt.Errorf("clock: non-positive denominator %d", den)
	}
	g := gcd(abs(num), den)
	return Rational{Num: num / g, Den: den / g}, nil
}

// Must is New for compile-time constants; it panics on bad input.
func Must(num, den int64) Rational {
	r, err := New(num, den)
	if err != nil {
		panic(err)
	}
	return r
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
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

// Add returns r+o, rejecting results that overflow int64.
func (r Rational) Add(o Rational) (Rational, error) {
	n := new(big.Int).Add(
		new(big.Int).Mul(big.NewInt(r.Num), big.NewInt(o.Den)),
		new(big.Int).Mul(big.NewInt(o.Num), big.NewInt(r.Den)))
	d := new(big.Int).Mul(big.NewInt(r.Den), big.NewInt(o.Den))
	if !n.IsInt64() || !d.IsInt64() {
		return Rational{}, ErrOverflow
	}
	return New(n.Int64(), d.Int64())
}

// Sub returns r-o, rejecting overflow.
func (r Rational) Sub(o Rational) (Rational, error) {
	return r.Add(Rational{Num: -o.Num, Den: o.Den})
}

// Cmp compares two rationals exactly: -1, 0 or +1.
func (r Rational) Cmp(o Rational) int {
	a := new(big.Int).Mul(big.NewInt(r.Num), big.NewInt(o.Den))
	b := new(big.Int).Mul(big.NewInt(o.Num), big.NewInt(r.Den))
	return a.Cmp(b)
}

// Float64 returns the approximate value in seconds.
func (r Rational) Float64() float64 { return float64(r.Num) / float64(r.Den) }

// Micros returns the value in microseconds, rounded down.
func (r Rational) Micros() (int64, error) {
	n := new(big.Int).Mul(big.NewInt(r.Num), big.NewInt(1_000_000))
	n.Div(n, big.NewInt(r.Den))
	if !n.IsInt64() {
		return 0, ErrOverflow
	}
	return n.Int64(), nil
}

func (r Rational) String() string { return fmt.Sprintf("%d/%d", r.Num, r.Den) }

// SignedDelta returns the shortest signed distance from ref to raw on a
// wrapping counter of the given period, in [-period/2, period/2).
// period must divide 2^64 (true for 2^32 and 2^33).
func SignedDelta(raw, ref, period uint64) int64 {
	d := (raw - ref) % period
	if d > period/2 {
		return int64(d) - int64(period)
	}
	return int64(d)
}

// Unwrap reconstructs a continuous int64 timeline from a wrapping counter.
type Unwrap struct {
	Period  uint64
	prevRaw uint64
	last    int64
	init    bool
}

// Next feeds a raw counter sample and returns the unwrapped value.
func (u *Unwrap) Next(raw uint64) int64 {
	raw %= u.Period
	if !u.init {
		u.init, u.prevRaw, u.last = true, raw, int64(raw)
		return u.last
	}
	u.last += SignedDelta(raw, u.prevRaw, u.Period)
	u.prevRaw = raw
	return u.last
}

// WrapPeriod returns the source counter period for a timescale:
// 33-bit for the 90 kHz MPEG clock, 32-bit otherwise.
func WrapPeriod(timescale uint32) uint64 {
	if timescale == 90000 {
		return 1 << 33
	}
	return 1 << 32
}
