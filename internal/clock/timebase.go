// Package clock defines the single rational media timebase used by every
// subsystem. All timestamps are integers; floating point is never used for
// accumulating time, only for display and quality metrics.
package clock

import (
	"errors"
	"fmt"
	"math/big"
	"time"
)

// Timebase is the duration of one tick expressed as a rational number of
// seconds: one tick == Num/Den seconds. Den is always positive and the
// fraction is reduced.
type Timebase struct {
	Num int64
	Den int64
}

// Common timebases used by fixtures and the timeline.
var (
	Millisecond = Timebase{Num: 1, Den: 1000}
	Video90k    = Timebase{Num: 1, Den: 90000}
)

var (
	ErrInvalidTimebase = errors.New("invalid timebase")
	ErrTimeOverflow    = errors.New("timestamp overflow")
)

// New validates and reduces a rational timebase.
func New(num, den int64) (Timebase, error) {
	if num <= 0 || den <= 0 {
		return Timebase{}, fmt.Errorf("%w: num=%d den=%d", ErrInvalidTimebase, num, den)
	}
	g := gcd(num, den)
	return Timebase{Num: num / g, Den: den / g}, nil
}

// MustNew is New for tests and static tables.
func MustNew(num, den int64) Timebase {
	tb, err := New(num, den)
	if err != nil {
		panic(err)
	}
	return tb
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	return a
}

func (t Timebase) String() string { return fmt.Sprintf("%d/%d", t.Num, t.Den) }

func (t Timebase) Valid() bool { return t.Num > 0 && t.Den > 0 }

// TicksForMillis converts a whole number of milliseconds to ticks.
func (t Timebase) TicksForMillis(ms int64) int64 {
	n, err := mulDiv(ms, t.Den, 1000*t.Num)
	if err != nil {
		return 0
	}
	return n
}

// TicksForDuration converts a time.Duration to ticks, rounding half away
// from zero. time.Duration is an exact integer nanosecond count.
func (t Timebase) TicksForDuration(d time.Duration) int64 {
	n, err := mulDiv(int64(d), t.Den, 1_000_000_000*t.Num)
	if err != nil {
		return 0
	}
	return n
}

// ToMillis converts ticks to milliseconds, rounding half away from zero.
func (t Timebase) ToMillis(ticks int64) int64 {
	ms, err := mulDiv(ticks, t.Num*1000, t.Den)
	if err != nil {
		return 0
	}
	return ms
}

// ToSeconds is presentation-only; it must not be used for accumulation.
func (t Timebase) ToSeconds(ticks int64) float64 {
	return float64(ticks) * float64(t.Num) / float64(t.Den)
}

// Convert converts ticks from t to dst without losing integer precision.
func (t Timebase) Convert(ticks int64, dst Timebase) (int64, error) {
	if !t.Valid() || !dst.Valid() {
		return 0, ErrInvalidTimebase
	}
	// ticks * (t.Num/t.Den) / (dst.Num/dst.Den)
	// == ticks * t.Num * dst.Den / (t.Den * dst.Num)
	return mulDiv(ticks, t.Num*dst.Den, t.Den*dst.Num)
}

// Add returns a+b in the same timebase. Both operands are interpreted in t.
func (t Timebase) Add(a, b int64) int64 { return a + b }

// mulDiv returns round(a*b/c) with half away from zero. math/big is used so
// the cross-products required by rational conversion cannot silently wrap.
func mulDiv(a, b, c int64) (int64, error) {
	if c == 0 {
		return 0, ErrInvalidTimebase
	}
	neg := (a < 0) != (b < 0)
	if c < 0 {
		c = -c
		neg = !neg
	}
	aa, bb, cc := big.NewInt(a), big.NewInt(b), big.NewInt(c)
	aa.Abs(aa)
	bb.Abs(bb)
	num := new(big.Int).Mul(aa, bb)
	q, r := new(big.Int).QuoRem(num, cc, new(big.Int))
	twice := new(big.Int).Lsh(r, 1)
	if twice.Cmp(cc) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if neg {
		q.Neg(q)
	}
	if !q.IsInt64() {
		return 0, ErrTimeOverflow
	}
	return q.Int64(), nil
}

// CeilDiv returns ceil(a/b) for b > 0 (used for buffer sizing).
func CeilDiv(a, b int64) int64 {
	if b <= 0 {
		return 0
	}
	if a <= 0 {
		return a / b
	}
	return (a + b - 1) / b
}
