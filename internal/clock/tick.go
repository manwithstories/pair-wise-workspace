// Package clock provides a single rational time scale shared by all
// media subsystems. Every timestamp in the pipeline is a Tick: a
// normalized rational number of seconds (Num/Den). Arithmetic is
// overflow-checked and reports ErrOverflow instead of wrapping.
package clock

import (
	"errors"
	"fmt"
	"math/big"
)

var (
	// ErrInvalidTimescale is returned when a zero time base is used.
	ErrInvalidTimescale = errors.New("clock: invalid time base")
	// ErrOverflow is returned when rational arithmetic exceeds int64.
	ErrOverflow = errors.New("clock: tick overflow")
)

// Tick is a rational number of seconds, normalized: Den > 0 and
// gcd(|Num|, Den) == 1.
type Tick struct {
	Num int64
	Den int64
}

// New builds a normalized Tick. A zero denominator is rejected.
func New(num, den int64) (Tick, error) {
	return norm(big.NewInt(num), big.NewInt(den))
}

// Must is New for compile-time constants; it panics on error.
func Must(num, den int64) Tick {
	t, err := New(num, den)
	if err != nil {
		panic(err)
	}
	return t
}

// Zero returns the zero tick.
func Zero() Tick { return Tick{Num: 0, Den: 1} }

func norm(num, den *big.Int) (Tick, error) {
	if den.Sign() == 0 {
		return Tick{}, ErrInvalidTimescale
	}
	if den.Sign() < 0 {
		num.Neg(num)
		den.Neg(den)
	}
	g := new(big.Int).GCD(nil, nil, new(big.Int).Abs(num), den)
	num.Quo(num, g)
	den.Quo(den, g)
	if !num.IsInt64() || !den.IsInt64() {
		return Tick{}, ErrOverflow
	}
	return Tick{Num: num.Int64(), Den: den.Int64()}, nil
}

// Add returns t+o, checked.
func (t Tick) Add(o Tick) (Tick, error) {
	n := new(big.Int).Mul(big.NewInt(t.Num), big.NewInt(o.Den))
	n.Add(n, new(big.Int).Mul(big.NewInt(o.Num), big.NewInt(t.Den)))
	d := new(big.Int).Mul(big.NewInt(t.Den), big.NewInt(o.Den))
	return norm(n, d)
}

// Sub returns t-o, checked.
func (t Tick) Sub(o Tick) (Tick, error) {
	return t.Add(Tick{Num: -o.Num, Den: o.Den})
}

// MulK returns t*k, checked.
func (t Tick) MulK(k int64) (Tick, error) {
	n := new(big.Int).Mul(big.NewInt(t.Num), big.NewInt(k))
	return norm(n, big.NewInt(t.Den))
}

// Cmp compares two ticks without overflow: -1, 0 or +1.
func (t Tick) Cmp(o Tick) int {
	l := new(big.Int).Mul(big.NewInt(t.Num), big.NewInt(o.Den))
	r := new(big.Int).Mul(big.NewInt(o.Num), big.NewInt(t.Den))
	return l.Cmp(r)
}

// Sign returns -1, 0 or +1.
func (t Tick) Sign() int {
	switch {
	case t.Num < 0:
		return -1
	case t.Num > 0:
		return 1
	default:
		return 0
	}
}

// Seconds returns the value as float64 seconds.
func (t Tick) Seconds() float64 {
	return float64(t.Num) / float64(t.Den)
}

// Millis returns the value as float64 milliseconds.
func (t Tick) Millis() float64 {
	return t.Seconds() * 1000
}

func (t Tick) String() string { return fmt.Sprintf("%d/%d", t.Num, t.Den) }
