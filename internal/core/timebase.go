// Package core defines the unified rational time base and the fault model
// shared by every presspack subsystem. All timeline arithmetic is done in
// integer ticks at a fixed 90 kHz time base so no floating point drift can
// accumulate across segments, subtitles or timecode conversions.
package core

import "fmt"

// Timescale is the global clock rate: 90000 ticks per second, matching the
// MPEG transport stream / CMAF media time base. Frame durations of every
// supported rate (24, 25, 30, 30000/1001 fps) are exact integers here:
// 3750, 3600, 3000 and 3003 ticks respectively.
const Timescale = 90000

// Rational is an exact fraction used for frame rates and source time bases.
type Rational struct {
	Num int64
	Den int64
}

// Rat builds a normalized Rational (denominator kept positive).
func Rat(num, den int64) Rational {
	if den < 0 {
		num, den = -num, -den
	}
	g := gcd(abs64(num), den)
	if g > 1 {
		num, den = num/g, den/g
	}
	return Rational{Num: num, Den: den}
}

// Time is an instant or duration expressed in global 90 kHz ticks.
type Time struct {
	Ticks int64
}

// FromUnits converts v units of a rational time base (seconds per unit =
// r.Num/r.Den) into global ticks, rounding to nearest.
func FromUnits(v int64, r Rational) Time {
	num := v * r.Num * Timescale
	den := r.Den
	if num >= 0 {
		return Time{Ticks: (num + den/2) / den}
	}
	return Time{Ticks: -((-num + den/2) / den)}
}

// FromMillis builds a Time from whole milliseconds, exactly.
func FromMillis(ms int64) Time { return Time{Ticks: ms * (Timescale / 1000)} }

// Add returns t+d.
func (t Time) Add(d Time) Time { return Time{Ticks: t.Ticks + d.Ticks} }

// Sub returns t-d.
func (t Time) Sub(d Time) Time { return Time{Ticks: t.Ticks - d.Ticks} }

// Cmp compares two times: -1, 0 or +1.
func (t Time) Cmp(o Time) int {
	switch {
	case t.Ticks < o.Ticks:
		return -1
	case t.Ticks > o.Ticks:
		return 1
	}
	return 0
}

// Millis returns the time in milliseconds, rounded to nearest.
func (t Time) Millis() int64 {
	if t.Ticks >= 0 {
		return (t.Ticks + Timescale/2000) / (Timescale / 1000)
	}
	return -((-t.Ticks + Timescale/2000) / (Timescale / 1000))
}

// Seconds returns a float view for display and metrics only; never feed the
// result back into timeline arithmetic.
func (t Time) Seconds() float64 { return float64(t.Ticks) / Timescale }

func (t Time) String() string {
	ms := t.Millis()
	sign := ""
	if ms < 0 {
		sign, ms = "-", -ms
	}
	return fmt.Sprintf("%s%02d:%02d:%02d.%03d", sign, ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
