// Package model defines the container-independent time base, track metadata
// and stable error classification shared by every MediaMeld subsystem.
package model

import (
	"fmt"
	"math/big"
	"sort"
)

// Instant is an integer nanosecond instant on the unified timeline. No float
// arithmetic is ever used to derive it, so repeated runs are bit-identical.
type Instant int64

func (i Instant) Milliseconds() int64 { return int64(i) / 1_000_000 }

func (i Instant) String() string { return fmt.Sprintf("%dns", int64(i)) }

// Rational is an exact fraction. Frame rates are always carried as Num/Den,
// never as a float.
type Rational struct {
	Num int64
	Den int64
}

// Ratio builds a normalized rational, rejecting a zero denominator.
func Ratio(num, den int64) (Rational, error) {
	if den == 0 {
		return Rational{}, fmt.Errorf("zero denominator")
	}
	if den < 0 {
		num, den = -num, -den
	}
	g := gcd(num, den)
	if g == 0 {
		g = 1
	}
	return Rational{Num: num / g, Den: den / g}, nil
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
	return a
}

func lcm(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	g := gcd(a, b)
	v := a / g * b
	if v < 0 {
		v = -v
	}
	return v
}

func (r Rational) String() string { return fmt.Sprintf("%d/%d", r.Num, r.Den) }

func (r Rational) Valid() error {
	if r.Num <= 0 || r.Den <= 0 {
		return fmt.Errorf("invalid rational %d/%d", r.Num, r.Den)
	}
	return nil
}

// Cadence turns a repeating cycle of frame rates into exact frame start ticks
// in an integer timescale. Frame n starts at floor(U(n)*timescale/D) where U
// is the cumulative numerator over the cycle's common denominator D. Keeping
// the numerator cumulative removes any per-frame rounding drift.
type Cadence struct {
	rates      []Rational
	units      []int64 // per-frame interval units over denom
	prefix     []int64 // prefix[i] = units of frames [0,i)
	cycleUnits int64
	denom      int64
}

// NewCadence validates and prepares a VFR rate cycle.
func NewCadence(rates []Rational) (Cadence, error) {
	if len(rates) == 0 {
		return Cadence{}, fmt.Errorf("empty cadence")
	}
	c := Cadence{rates: append([]Rational(nil), rates...)}
	c.denom = 1
	for _, r := range rates {
		if err := r.Valid(); err != nil {
			return Cadence{}, err
		}
		c.denom = lcm(c.denom, r.Num)
	}
	c.units = make([]int64, len(rates))
	c.prefix = make([]int64, len(rates)+1)
	for i, r := range rates {
		// interval = Den/Num seconds; in units of 1/denom second:
		// units = Den * (denom/Num).
		c.units[i] = r.Den * (c.denom / r.Num)
		c.prefix[i+1] = c.prefix[i] + c.units[i]
	}
	c.cycleUnits = c.prefix[len(rates)]
	if c.cycleUnits <= 0 {
		return Cadence{}, fmt.Errorf("non-positive cadence cycle")
	}
	return c, nil
}

// Start returns the exact start tick of frame index frame (0-based).
func (c Cadence) Start(frame int64, timescale int64) int64 {
	if frame < 0 {
		frame = 0
	}
	n := int64(len(c.rates))
	cycles, rem := frame/n, int(frame%n)
	units := cycles*c.cycleUnits + c.prefix[rem]
	return units * timescale / c.denom
}

// Duration returns frame duration in ticks as the difference of two exact
// frame starts. It may be zero for a timescale too coarse to resolve a frame.
func (c Cadence) Duration(frame int64, timescale int64) int64 {
	return c.Start(frame+1, timescale) - c.Start(frame, timescale)
}

// Clock converts source ticks into unified nanoseconds while compensating a
// measured source-clock drift in parts per million. A positive drift means the
// source clock runs fast, so true elapsed time is compressed.
type Clock struct {
	Timescale int64
	DriftPPM  int64
}

const ppmScale = 1_000_000

func (c Clock) Validate() error {
	if c.Timescale <= 0 {
		return fmt.Errorf("clock timescale must be positive")
	}
	if c.DriftPPM <= -ppmScale || c.DriftPPM > ppmScale {
		return fmt.Errorf("drift ppm out of range: %d", c.DriftPPM)
	}
	return nil
}

// Calibrate converts source ticks to nanoseconds with exact integer
// arithmetic. Truncation is toward zero and monotone, so ordering is stable.
func (c Clock) Calibrate(ticks int64) Instant {
	if c.Timescale <= 0 {
		return 0
	}
	num := big.NewInt(ticks)
	num.Mul(num, big.NewInt(1_000_000_000))
	num.Mul(num, big.NewInt(ppmScale))
	den := big.NewInt(c.Timescale * (ppmScale + c.DriftPPM))
	if den.Sign() == 0 {
		return 0
	}
	return Instant(new(big.Int).Quo(num, den).Int64())
}

// Unwrap32 extends 32-bit timestamps into 64-bit by choosing the candidate
// nearest a high-water reference. The high-water mark only moves forward so a
// reordered packet observed around the wrap point still lands on the correct
// side of the modulus.
func Unwrap32(values []uint32) []int64 {
	const mod = int64(1) << 32
	out := make([]int64, len(values))
	var high int64
	seen := false
	for i, v := range values {
		base := high &^ (mod - 1)
		cand := base | int64(v)
		if seen {
			if cand-high > mod/2 {
				cand -= mod
			} else if high-cand > mod/2 {
				cand += mod
			}
		}
		out[i] = cand
		if !seen || cand > high {
			high = cand
			seen = true
		}
	}
	return out
}

// SortTicks returns a sorted copy of the given ticks.
func SortTicks(v []int64) []int64 {
	out := append([]int64(nil), v...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
