// Package clock provides an exact rational time base shared by scanning,
// streaming and RTP scheduling. All conversions are checked for overflow and
// the round-trip error is bounded below one millisecond.
package clock

import (
	"errors"
	"fmt"
	"math/big"
)

// ErrOverflow is returned whenever a conversion would not fit in int64.
var ErrOverflow = errors.New("clock: integer overflow")

// Rational is an exact non-negative time quantity: Num/Den seconds.
// Den is always > 0 and Num is always >= 0.
type Rational struct {
	Num int64
	Den int64
}

// NewRational validates and normalises a rational time value.
func NewRational(num, den int64) (Rational, error) {
	if den == 0 {
		return Rational{}, fmt.Errorf("clock: zero denominator")
	}
	if den < 0 {
		num, den = -num, -den
	}
	if num < 0 {
		return Rational{}, fmt.Errorf("clock: negative time %d/%d", num, den)
	}
	g := gcd(num, den)
	if g > 1 {
		num /= g
		den /= g
	}
	return Rational{Num: num, Den: den}, nil
}

// FromTicks converts a media clock tick count at the given timescale into a
// Rational. ticks may be zero but not negative.
func FromTicks(ticks int64, timescale uint32) (Rational, error) {
	if timescale == 0 {
		return Rational{}, fmt.Errorf("clock: zero timescale")
	}
	if ticks < 0 {
		return Rational{}, fmt.Errorf("clock: negative ticks %d", ticks)
	}
	return NewRational(ticks, int64(timescale))
}

// FromSeconds converts a float seconds value, rounding to nanoseconds. This is
// only used at API boundaries where the caller already works in float.
func FromSeconds(sec float64) (Rational, error) {
	if sec < 0 {
		return Rational{}, fmt.Errorf("clock: negative seconds %f", sec)
	}
	ns := int64(sec*1e9 + 0.5)
	return NewRational(ns, 1e9)
}

// Add returns a+b, reporting overflow.
func (r Rational) Add(b Rational) (Rational, error) {
	a, err := r.toBig()
	if err != nil {
		return Rational{}, err
	}
	c, err := b.toBig()
	if err != nil {
		return Rational{}, err
	}
	return fromBig(new(big.Rat).Add(a, c))
}

// Sub returns a-b; a must be >= b (time is monotonic in this pipeline).
func (r Rational) Sub(b Rational) (Rational, error) {
	a, err := r.toBig()
	if err != nil {
		return Rational{}, err
	}
	c, err := b.toBig()
	if err != nil {
		return Rational{}, err
	}
	if a.Cmp(c) < 0 {
		return Rational{}, fmt.Errorf("clock: negative duration %s - %s", r, b)
	}
	return fromBig(new(big.Rat).Sub(a, c))
}

// MulInt multiplies by an integer factor with overflow checks.
func (r Rational) MulInt(n int64) (Rational, error) {
	if n < 0 {
		return Rational{}, fmt.Errorf("clock: negative multiplier %d", n)
	}
	if r.Num != 0 && n > 0 && r.Num > (1<<62)/n {
		return Rational{}, ErrOverflow
	}
	return NewRational(r.Num*n, r.Den)
}

// Cmp compares two rationals: -1, 0 or 1.
func (r Rational) Cmp(b Rational) int {
	a, err := r.toBig()
	if err != nil {
		return 0
	}
	c, err := b.toBig()
	if err != nil {
		return 0
	}
	return a.Cmp(c)
}

// Seconds renders the value as float64 seconds.
func (r Rational) Seconds() float64 {
	return float64(r.Num) / float64(r.Den)
}

// Milliseconds renders the value as float64 milliseconds.
func (r Rational) Milliseconds() float64 { return r.Seconds() * 1000 }

// Nanos returns the truncated nanosecond representation if it fits.
func (r Rational) Nanos() (int64, error) {
	q := new(big.Int).SetInt64(r.Num)
	q.Mul(q, big.NewInt(1e9))
	q.Quo(q, big.NewInt(r.Den))
	if !q.IsInt64() {
		return 0, ErrOverflow
	}
	return q.Int64(), nil
}

// String renders the value as an exact fraction.
func (r Rational) String() string { return fmt.Sprintf("%d/%d", r.Num, r.Den) }

// ToTicksOf converts the value into integer ticks at timescale, rounding
// half-up, and reports overflow.
func (r Rational) ToTicksOf(timescale uint32) (int64, error) {
	if timescale == 0 {
		return 0, fmt.Errorf("clock: zero timescale")
	}
	q := new(big.Int).SetInt64(r.Num)
	q.Mul(q, big.NewInt(int64(timescale)))
	q.Add(q, big.NewInt(r.Den/2))
	q.Quo(q, big.NewInt(r.Den))
	if !q.IsInt64() {
		return 0, ErrOverflow
	}
	return q.Int64(), nil
}

// Timebase is an exact rational time base rooted at a non-zero presentation
// timestamp. It converts between the media timescale and wall-clock seconds.
type Timebase struct {
	// Timescale is the media clock frequency in Hz.
	Timescale uint32
	// Origin is the presentation offset of the first sample on the timeline.
	Origin Rational
}

// NewTimebase builds a time base with a validated origin.
func NewTimebase(timescale uint32, originTicks int64) (Timebase, error) {
	if timescale == 0 {
		return Timebase{}, fmt.Errorf("clock: zero timescale")
	}
	origin, err := FromTicks(originTicks, timescale)
	if err != nil {
		return Timebase{}, err
	}
	return Timebase{Timescale: timescale, Origin: origin}, nil
}

// At converts a tick offset (relative to the media start) into absolute
// timeline seconds, adding the origin.
func (tb Timebase) At(ticks int64) (Rational, error) {
	rel, err := FromTicks(ticks, tb.Timescale)
	if err != nil {
		return Rational{}, err
	}
	return rel.Add(tb.Origin)
}

// Duration converts a tick span into a timeline duration.
func (tb Timebase) Duration(ticks int64) (Rational, error) {
	return FromTicks(ticks, tb.Timescale)
}

// ToTicks converts absolute timeline seconds into media ticks, rounding
// half-up. Values before the origin are clamped to zero.
func (tb Timebase) ToTicks(abs Rational) (int64, error) {
	rel, err := abs.Sub(tb.Origin)
	if err != nil {
		if abs.Cmp(tb.Origin) < 0 {
			return 0, nil
		}
		return 0, err
	}
	return rel.ToTicksOf(tb.Timescale)
}

// RoundTripErrorMillis measures |At(ToTicks(x)) - x| in milliseconds; it is
// used by tests and diagnostics to prove the sub-millisecond guarantee.
func (tb Timebase) RoundTripErrorMillis(abs Rational) (float64, error) {
	ticks, err := tb.ToTicks(abs)
	if err != nil {
		return 0, err
	}
	back, err := tb.At(ticks)
	if err != nil {
		return 0, err
	}
	var d Rational
	if back.Cmp(abs) >= 0 {
		d, err = back.Sub(abs)
	} else {
		d, err = abs.Sub(back)
	}
	if err != nil {
		return 0, err
	}
	return d.Milliseconds(), nil
}

func (r Rational) toBig() (*big.Rat, error) {
	if r.Den <= 0 {
		return nil, fmt.Errorf("clock: invalid denominator %d", r.Den)
	}
	return big.NewRat(r.Num, r.Den), nil
}

func fromBig(b *big.Rat) (Rational, error) {
	if b.Sign() < 0 {
		return Rational{}, fmt.Errorf("clock: negative rational result")
	}
	num := b.Num()
	den := b.Denom()
	if !num.IsInt64() || !den.IsInt64() {
		return Rational{}, ErrOverflow
	}
	return NewRational(num.Int64(), den.Int64())
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
