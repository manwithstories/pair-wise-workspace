// Package clock provides exact rational media clocks.
//
// A Timebase describes how many ticks make up one second: a single tick lasts
// Num/Den seconds.  Audio sample clocks are therefore Audio(sampleRate) and
// video frame clocks are Video(fpsNum, fpsDen), so 29.97 fps becomes the
// 1001/30000 tick clock and 25 fps becomes 1/25.
//
// Every conversion here keeps the full rational value; no float64 is used on
// the conversion path.  Conversions round to the nearest integer, and
// Accumulator carries the sub-nanosecond residue forward, so a long running
// re-calibration stays below one nanosecond of accumulated error instead of
// drifting with the packet count.
package clock

import (
	"errors"
	"fmt"
	"math/big"
	"math/bits"
	"time"
)

// ErrInvalidTimebase is returned for timebases with a zero part.
var ErrInvalidTimebase = errors.New("clock: invalid timebase")

// Timebase is a rational tick clock: one tick lasts Num/Den seconds.
type Timebase struct {
	Num int64
	Den int64
}

// New returns the exact timebase Num/Den without normalising it.
func New(num, den int64) (Timebase, error) {
	if num <= 0 || den <= 0 {
		return Timebase{}, ErrInvalidTimebase
	}
	return Timebase{Num: num, Den: den}, nil
}

// MustNew is New for static configuration where failure is a programming
// error.
func MustNew(num, den int64) Timebase {
	tb, err := New(num, den)
	if err != nil {
		panic(err)
	}
	return tb
}

// Audio builds a sample clock for a sample rate in Hz.
func Audio(hz int64) Timebase { return MustNew(1, hz) }

// Video builds a frame clock from a rational frame rate, so 30000/1001 yields
// the 1001/30000 tick timebase.
func Video(num, den int64) Timebase { return MustNew(den, num) }

// Valid reports whether the timebase can be used for conversions.
func (tb Timebase) Valid() bool { return tb.Num > 0 && tb.Den > 0 }

// String renders the exact rational in FFmpeg notation.
func (tb Timebase) String() string { return fmt.Sprintf("%d/%d", tb.Num, tb.Den) }

// Equal compares two rationals by cross multiplication.
func (tb Timebase) Equal(other Timebase) bool {
	return tb.Num*other.Den == other.Num*tb.Den
}

// FloatSeconds is for display only; never feed it back into media math.
func (tb Timebase) FloatSeconds(ticks int64) float64 {
	return float64(ticks) * float64(tb.Num) / float64(tb.Den)
}

// Nanos converts ticks to nanoseconds, rounding to nearest.
func (tb Timebase) Nanos(ticks int64) int64 {
	if !tb.Valid() {
		return 0
	}
	return mulDivRound(ticks, tb.Num*1_000_000_000, tb.Den)
}

// Duration is Nanos as a time.Duration.
func (tb Timebase) Duration(ticks int64) time.Duration {
	return time.Duration(tb.Nanos(ticks))
}

// Ticks converts a duration to ticks, rounding to nearest.
func (tb Timebase) Ticks(d time.Duration) int64 {
	if !tb.Valid() {
		return 0
	}
	return mulDivRound(int64(d), tb.Den, tb.Num*1_000_000_000)
}

// Rescale converts ticks from tb to dst, rounding to nearest.
func (tb Timebase) Rescale(ticks int64, dst Timebase) int64 {
	if !tb.Valid() || !dst.Valid() {
		return 0
	}
	return mulDivRound(ticks, dst.Den*tb.Num, dst.Num*tb.Den)
}

// Accumulator sums tick durations exactly by carrying the residue expressed in
// the source timebase denominator.
type Accumulator struct {
	timebase Timebase
	ticks    int64
	remNanos int64 // always < timebase.Den
}

// NewAccumulator returns an accumulator for tb.
func NewAccumulator(tb Timebase) *Accumulator { return &Accumulator{timebase: tb} }

// Add includes ticks in the accumulated span and returns the elapsed time.
// The exact sub-nanosecond remainder stays in the accumulator.
func (a *Accumulator) Add(ticks int64) time.Duration {
	if !a.timebase.Valid() || ticks < 0 {
		return a.Elapsed()
	}
	a.ticks += ticks
	return a.Elapsed()
}

// Elapsed returns the exact elapsed time of all ticks added so far.
func (a *Accumulator) Elapsed() time.Duration {
	if !a.timebase.Valid() {
		return 0
	}
	// nanos = (ticks*Num*1e9 + rem) / Den, with the remainder carried in
	// Den units.
	hi, lo := bits.Mul64(uint64(a.ticks), uint64(a.timebase.Num)*1_000_000_000)
	lo, carry := bits.Add64(lo, uint64(a.remNanos), 0)
	hi += carry
	den := uint64(a.timebase.Den)
	if hi >= den {
		// Enormous values: fall back to arbitrary precision rather than
		// calling bits.Div64 with a quotient that does not fit in 64 bits.
		total := new(big.Int).SetUint64(hi)
		total.Lsh(total, 64)
		total.Or(total, new(big.Int).SetUint64(lo))
		quo, rem := new(big.Int).QuoRem(total, new(big.Int).SetUint64(den), new(big.Int))
		a.remNanos = rem.Int64()
		return time.Duration(quo.Int64())
	}
	quo, rem := bits.Div64(hi, lo, den)
	a.remNanos = int64(rem)
	return time.Duration(quo)
}

// Ticks returns the raw tick sum.
func (a *Accumulator) Ticks() int64 { return a.ticks }

// Timebase returns the source timebase.
func (a *Accumulator) Timebase() Timebase { return a.timebase }

// Reset clears the accumulator.
func (a *Accumulator) Reset() { a.ticks, a.remNanos = 0, 0 }

// mulDivRound computes a*b/c rounded to nearest with a 128 bit intermediate.
func mulDivRound(a, b, c int64) int64 {
	if c == 0 {
		return 0
	}
	negA, negB, negC := a < 0, b < 0, c < 0
	ua, ub, uc := uint64(abs64(a)), uint64(abs64(b)), uint64(abs64(c))
	hi, lo := bits.Mul64(ua, ub)
	var q, r uint64
	if hi < uc {
		q, r = bits.Div64(hi, lo, uc)
	} else {
		p := new(big.Int).SetUint64(hi)
		p.Lsh(p, 64)
		p.Or(p, new(big.Int).SetUint64(lo))
		qb, rb := new(big.Int).QuoRem(p, new(big.Int).SetUint64(uc), new(big.Int))
		q, r = qb.Uint64(), rb.Uint64()
	}
	if r*2 >= uc {
		q++
	}
	if (negA != negB) != negC {
		return -int64(q)
	}
	return int64(q)
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
