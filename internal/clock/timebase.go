// Package clock provides exact integer rational time conversion. No floating
// point value ever participates in timestamp arithmetic.
package clock

import (
	"fmt"
	"math"
	"math/big"
)

// Rational is an exact numerator/denominator pair. It represents the size of
// one tick in seconds: Num/Den.
type Rational struct {
	Num int64
	Den int64
}

// New validates and normalizes a rational.
func New(num, den int64) (Rational, error) {
	if den == 0 {
		return Rational{}, fmt.Errorf("clock: zero denominator")
	}
	if num < 0 || den < 0 {
		return Rational{}, fmt.Errorf("clock: negative rational %d/%d", num, den)
	}
	if num == 0 {
		return Rational{}, fmt.Errorf("clock: zero numerator")
	}
	g := gcd(num, den)
	return Rational{Num: num / g, Den: den / g}, nil
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

// FromTimescale returns the tick duration of an MP4 timescale.
func FromTimescale(timescale uint32) (Rational, error) {
	if timescale == 0 {
		return Rational{}, fmt.Errorf("clock: zero timescale")
	}
	return New(1, int64(timescale))
}

// scale multiplies v by mul and divides by div exactly, reporting overflow.
func scale(v uint64, mul, div uint64) (uint64, error) {
	if div == 0 {
		return 0, fmt.Errorf("clock: division by zero")
	}
	z := new(big.Int).SetUint64(v)
	z.Mul(z, new(big.Int).SetUint64(mul))
	z.Quo(z, new(big.Int).SetUint64(div))
	if !z.IsUint64() {
		return 0, fmt.Errorf("clock: overflow converting %d*%d/%d", v, mul, div)
	}
	return z.Uint64(), nil
}

// Scale converts v ticks at fromTimescale into ticks at toTimescale.
func Scale(v uint64, fromTimescale, toTimescale uint32) (uint64, error) {
	if fromTimescale == 0 || toTimescale == 0 {
		return 0, fmt.Errorf("clock: zero timescale (%d -> %d)", fromTimescale, toTimescale)
	}
	return scale(v, uint64(toTimescale), uint64(fromTimescale))
}

// Nanos converts ticks at timescale into integer nanoseconds, truncating any
// sub-nanosecond remainder.
func Nanos(v uint64, timescale uint32) (uint64, error) {
	if timescale == 0 {
		return 0, fmt.Errorf("clock: zero timescale")
	}
	return scale(v, 1_000_000_000, uint64(timescale))
}

// Micros converts ticks at timescale into integer microseconds.
func Micros(v uint64, timescale uint32) (uint64, error) {
	if timescale == 0 {
		return 0, fmt.Errorf("clock: zero timescale")
	}
	return scale(v, 1_000_000, uint64(timescale))
}

// Add returns a+b, reporting overflow.
func Add(a, b uint64) (uint64, error) {
	if math.MaxUint64-a < b {
		return 0, fmt.Errorf("clock: addition overflow %d+%d", a, b)
	}
	return a + b, nil
}

// Monotonic detects decreasing timestamps. Equal timestamps are accepted.
type Monotonic struct {
	started bool
	last    uint64
}

// Observe feeds the next timestamp in decode order.
func (m *Monotonic) Observe(v uint64) error {
	if m.started && v < m.last {
		return fmt.Errorf("clock: non-monotonic timestamp %d after %d", v, m.last)
	}
	m.started = true
	m.last = v
	return nil
}

// Last returns the most recently observed timestamp.
func (m *Monotonic) Last() uint64 { return m.last }
