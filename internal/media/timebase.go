// Package media contains the shared rational clock and structured fault model
// used by every processing stage.
package media

import (
	"errors"
	"fmt"
	"math/big"
)

// Rational is an immutable signed rational number. Denominator is always > 0.
type Rational struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

// NewRational normalizes num/den. A zero denominator is rejected.
func NewRational(num, den int64) (Rational, error) {
	if den == 0 {
		return Rational{}, errors.New("rational denominator is zero")
	}
	if den < 0 {
		if num == -1<<63 {
			return Rational{}, errors.New("integer overflow while normalizing rational sign")
		}
		num, den = -num, -den
	}
	if num == 0 {
		return Rational{Num: 0, Den: 1}, nil
	}
	a := num
	if a < 0 {
		a = -a
	}
	g := gcd(a, den)
	return Rational{Num: num / g, Den: den / g}, nil
}

func MustRational(num, den int64) Rational {
	r, err := NewRational(num, den)
	if err != nil {
		panic(err)
	}
	return r
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// IsZero reports whether r represents zero.
func (r Rational) IsZero() bool { return r.Num == 0 }

// Cmp compares two rationals without floating-point rounding.
func (r Rational) Cmp(other Rational) int {
	l := new(big.Int).Mul(big.NewInt(r.Num), big.NewInt(other.Den))
	rr := new(big.Int).Mul(big.NewInt(other.Num), big.NewInt(r.Den))
	return l.Cmp(rr)
}

func (r Rational) String() string { return fmt.Sprintf("%d/%d", r.Num, r.Den) }

// Clock maps integer media ticks to an exact rational time unit.
// SecondsPerTick is normally 1/timescale.
type Clock struct {
	SecondsPerTick Rational `json:"seconds_per_tick"`
}

// NewClock creates a clock with the supplied timescale (ticks per second).
func NewClock(timescale uint32) (Clock, error) {
	if timescale == 0 {
		return Clock{}, errors.New("timescale must not be zero")
	}
	return Clock{SecondsPerTick: MustRational(1, int64(timescale))}, nil
}

func (c Clock) Timescale() int64 { return c.SecondsPerTick.Den }

// TicksToNanoseconds converts ticks with overflow-safe arbitrary arithmetic.
func (c Clock) TicksToNanoseconds(ticks int64) (int64, error) {
	// ticks * Num / Den * 1e9
	n := new(big.Int).Mul(big.NewInt(ticks), big.NewInt(c.SecondsPerTick.Num))
	n.Mul(n, big.NewInt(1_000_000_000))
	d := big.NewInt(c.SecondsPerTick.Den)
	n.Quo(n, d)
	if !n.IsInt64() {
		return 0, fmt.Errorf("nanosecond conversion overflow for ticks=%d clock=%s", ticks, c.SecondsPerTick)
	}
	return n.Int64(), nil
}

// NanosecondsToTicks rounds to the nearest representable tick.
func (c Clock) NanosecondsToTicks(ns int64) (int64, error) {
	// ns * Den / (Num * 1e9), rounded half up.
	if c.SecondsPerTick.Num == 0 {
		return 0, errors.New("clock numerator is zero")
	}
	num := new(big.Int).Mul(big.NewInt(ns), big.NewInt(c.SecondsPerTick.Den))
	den := new(big.Int).Mul(big.NewInt(c.SecondsPerTick.Num), big.NewInt(1_000_000_000))
	if den.Sign() == 0 {
		return 0, errors.New("clock conversion denominator is zero")
	}
	// Work with absolute value for symmetric half-away-from-zero rounding.
	neg := num.Sign() < 0
	absNum := new(big.Int).Abs(num)
	half := new(big.Int).Rsh(den, 1)
	absNum.Add(absNum, half)
	q := absNum.Quo(absNum, den)
	if neg {
		q.Neg(q)
	}
	if !q.IsInt64() {
		return 0, fmt.Errorf("tick conversion overflow for ns=%d", ns)
	}
	return q.Int64(), nil
}

// ConvertTicks maps a tick value from one clock to another using nearest rounding.
func ConvertTicks(ticks int64, from, to Clock) (int64, error) {
	ns, err := from.TicksToNanoseconds(ticks)
	if err != nil {
		return 0, err
	}
	return to.NanosecondsToTicks(ns)
}

// AbsDiffNS returns the absolute nanosecond difference between two tick values.
func AbsDiffNS(aTicks, bTicks int64, clock Clock) (int64, error) {
	a, err := clock.TicksToNanoseconds(aTicks)
	if err != nil {
		return 0, err
	}
	b, err := clock.TicksToNanoseconds(bTicks)
	if err != nil {
		return 0, err
	}
	if a < b {
		a, b = b, a
	}
	return a - b, nil
}
