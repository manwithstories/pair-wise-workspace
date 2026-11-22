// Package timebase unifies every time value in the system — PTS, DTS,
// durations, timestamps — as exact rational numbers instead of float. All
// cross-stage timing math (ingest, metadata resolution, manifest alignment,
// remux) goes through the types here so rounding error cannot accumulate.
package timebase

import (
	"fmt"
	"math/big"

	"mediarun/internal/mediaerr"
)

// Rational is an exact signed rational number Num/Den with Den > 0.
type Rational struct {
	Num int64
	Den int64
}

// New builds a rational and normalizes the denominator to be positive.
// It panics on den == 0, which is always a programming error rather than
// a recoverable input condition.
func New(num, den int64) Rational {
	if den == 0 {
		panic("timebase: zero denominator")
	}
	if den < 0 {
		num, den = -num, -den
	}
	g := gcd(abs64(num), den)
	if g > 1 {
		num /= g
		den /= g
	}
	return Rational{Num: num, Den: den}
}

// FromInt promotes an integer to a rational.
func FromInt(v int64) Rational { return Rational{Num: v, Den: 1} }

// Zero is the canonical zero value (normalized to 0/1 so Cmp works).
var Zero = Rational{Num: 0, Den: 1}

func (r Rational) String() string {
	if r.Den == 1 || r.Den == 0 {
		return fmt.Sprintf("%d", r.Num)
	}
	return fmt.Sprintf("%d/%d", r.Num, r.Den)
}

// IsZero reports whether the value equals zero.
func (r Rational) IsZero() bool { return r.Num == 0 }

// IsNegative reports whether the value is below zero; used to reject
// negative durations with a structured error at the boundary.
func (r Rational) IsNegative() bool { return r.Num < 0 }

// IsPositive reports whether the value is strictly above zero.
func (r Rational) IsPositive() bool { return r.Num > 0 }

// Cmp returns -1/0/1 comparing r and o exactly.
func (r Rational) Cmp(o Rational) int {
	rd, od := r.Den, o.Den
	if rd == 0 { // defensive: a bare Rational{} means zero
		rd = 1
	}
	if od == 0 {
		od = 1
	}
	a := big.NewInt(r.Num)
	a.Mul(a, big.NewInt(od))
	b := big.NewInt(o.Num)
	b.Mul(b, big.NewInt(rd))
	return a.Cmp(b)
}

// Equal compares two rationals exactly.
func (r Rational) Equal(o Rational) bool { return r.Cmp(o) == 0 }

// Add returns r+o exactly.
func (r Rational) Add(o Rational) Rational {
	return New(r.Num*o.Den+o.Num*r.Den, r.Den*o.Den)
}

// Sub returns r-o exactly.
func (r Rational) Sub(o Rational) Rational {
	return New(r.Num*o.Den-o.Num*r.Den, r.Den*o.Den)
}

// Mul returns r*o exactly.
func (r Rational) Mul(o Rational) Rational {
	return New(r.Num*o.Num, r.Den*o.Den)
}

// Div returns r/o exactly; panics on division by zero.
func (r Rational) Div(o Rational) Rational {
	return New(r.Num*o.Den, r.Den*o.Num)
}

// Float64 is for statistics/reporting only; timing decisions never use it.
func (r Rational) Float64() float64 {
	if r.Den == 0 {
		return 0
	}
	return float64(r.Num) / float64(r.Den)
}

// RequireNonNegative maps a negative duration/offset to the structured
// NEGATIVE_DURATION error instead of letting it flow downstream.
func RequireNonNegative(r Rational, track string) error {
	if r.IsNegative() {
		return mediaerr.New(mediaerr.CodeNegativeDuration,
			fmt.Sprintf("negative duration %s on track %s", r, track)).
			At("", track, -1)
	}
	return nil
}

// Timebase expresses ticks-per-second of a track clock, e.g. 90000/1 for
// an MPEG-TS video clock or 48000/1 for PCM audio.
type Timebase struct {
	r Rational
}

// NewTimebase builds a ticks-per-second timebase.
func NewTimebase(ticksPerSecond int64) Timebase {
	return Timebase{r: New(ticksPerSecond, 1)}
}

// NewTimebaseRat builds a fractional timebase, e.g. 30000/1001.
func NewTimebaseRat(num, den int64) Timebase {
	return Timebase{r: New(num, den)}
}

// TicksPerSecond returns the underlying rational rate.
func (t Timebase) TicksPerSecond() Rational { return t.r }

// Tick is the duration of one clock tick, in seconds.
func (t Timebase) Tick() Rational { return New(1, 1).Div(t.r) }

// ToSeconds converts a tick count on this clock to exact seconds.
func (t Timebase) ToSeconds(ticks int64) Rational {
	return New(ticks, 1).Mul(t.Tick())
}

// FromSeconds quantizes a rational-second value to whole ticks, rounding
// half-up; the residual is returned so callers can accumulate it.
func (t Timebase) FromSeconds(sec Rational) (ticks int64, residual Rational) {
	q := sec.Mul(t.r) // exact tick count, possibly fractional
	whole := q.Num / q.Den
	rem := New(q.Num%q.Den, q.Den)
	if rem.IsNegative() {
		whole--
		rem = rem.Add(FromInt(1))
	}
	if rem.Cmp(New(1, 2)) >= 0 {
		whole++
		rem = rem.Sub(FromInt(1))
	}
	return whole, rem
}

// Millis rounds a rational-second value to whole milliseconds for reports.
func Millis(sec Rational) int64 {
	tb := NewTimebase(1000)
	v, _ := tb.FromSeconds(sec)
	return v
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
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
