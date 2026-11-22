// Package clock provides the unified time base used across all
// representations. All arithmetic is exact rational arithmetic; any
// conversion that would lose precision is reported, never silently
// rounded, so independently segmented bitrates cannot drift apart.
package clock

import (
	"fmt"
	"math/big"
)

// Rational is an exact media timestamp: Value/Timescale seconds.
type Rational struct {
	Value     int64 `json:"value"`
	Timescale int64 `json:"timescale"`
}

// New returns v/ts normalized by their greatest common divisor.
func New(v, ts int64) Rational {
	if ts <= 0 {
		panic("clock: non-positive timescale")
	}
	g := GCD(abs(v), ts)
	return Rational{Value: v / g, Timescale: ts / g}
}

// Add returns a+b exactly.
func (a Rational) Add(b Rational) Rational {
	ts := LCM(a.Timescale, b.Timescale)
	return New(a.Value*(ts/a.Timescale)+b.Value*(ts/b.Timescale), ts)
}

// Sub returns a-b exactly.
func (a Rational) Sub(b Rational) Rational {
	ts := LCM(a.Timescale, b.Timescale)
	return New(a.Value*(ts/a.Timescale)-b.Value*(ts/b.Timescale), ts)
}

// Cmp compares a and b exactly (-1, 0, +1). big.Int cross
// multiplication avoids any overflow or rounding.
func (a Rational) Cmp(b Rational) int {
	x := new(big.Int).Mul(big.NewInt(a.Value), big.NewInt(b.Timescale))
	y := new(big.Int).Mul(big.NewInt(b.Value), big.NewInt(a.Timescale))
	return x.Cmp(y)
}

// IsZero reports whether the timestamp is exactly zero.
func (a Rational) IsZero() bool { return a.Value == 0 }

// MillisExact converts to whole milliseconds. ok is false when the
// timestamp is not an exact number of milliseconds; callers must not
// fall back to silent rounding.
func (a Rational) MillisExact() (ms int64, ok bool) {
	num := a.Value * 1000
	if num%a.Timescale != 0 {
		return 0, false
	}
	return num / a.Timescale, true
}

// MillisFloor returns floor(seconds*1000); used only for diagnostics
// where an explicit lower bound is wanted.
func (a Rational) MillisFloor() int64 { return a.Value * 1000 / a.Timescale }

// Aligned reports whether the timestamp lands exactly on a grid of
// `units` ticks in its own timescale (e.g. an audio frame boundary).
func (a Rational) Aligned(units int64) bool { return a.Value%units == 0 }

func (a Rational) String() string { return fmt.Sprintf("%d/%d", a.Value, a.Timescale) }

// GCD returns the greatest common divisor of a and b.
func GCD(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// LCM returns the least common multiple of a and b.
func LCM(a, b int64) int64 { return a / GCD(a, b) * b }

// CommonTimescale returns the LCM of all given timescales.
func CommonTimescale(ts ...int64) int64 {
	out := int64(1)
	for _, t := range ts {
		out = LCM(out, t)
	}
	return out
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
