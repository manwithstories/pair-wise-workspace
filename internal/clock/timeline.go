// Package clock implements the shared 90 kHz integer clock and exact
// rational conversions between media timescales. All arithmetic is
// integer-only; no floating point accumulation is ever used.
package clock

import "math/bits"

// Rate is the shared timeline rate: 90 000 ticks per second.
const Rate = 90000

// MulDiv returns a*b/c truncated toward zero. The 128-bit intermediate
// keeps the result exact for the full int64 domain of media timestamps.
func MulDiv(a, b, c int64) int64 {
	if c == 0 {
		panic("clock: division by zero")
	}
	neg := (a < 0) != (b < 0) != (c < 0)
	ua, ub, uc := abs64(a), abs64(b), abs64(c)
	hi, lo := bits.Mul64(ua, ub)
	q, _ := bits.Div64(hi, lo, uc) // panics only on quotient overflow (>int64 range)
	if neg {
		return -int64(q)
	}
	return int64(q)
}

func abs64(v int64) uint64 {
	if v < 0 {
		return uint64(-(v + 1)) + 1
	}
	return uint64(v)
}

// Timeline converts between a media timescale and the shared 90 kHz clock.
type Timeline struct {
	Timescale uint32
}

// Valid reports whether the timescale is usable.
func (t Timeline) Valid() bool { return t.Timescale > 0 }

// ToClock maps media-timeline ticks to shared 90 kHz clock ticks.
// It converts absolute positions (never deltas), so no error accumulates.
func (t Timeline) ToClock(mediaTicks int64) int64 {
	return MulDiv(mediaTicks, Rate, int64(t.Timescale))
}

// FromClock maps shared 90 kHz clock ticks back to media-timeline ticks.
func (t Timeline) FromClock(clockTicks int64) int64 {
	return MulDiv(clockTicks, int64(t.Timescale), Rate)
}

// SecondsToClock converts whole seconds to shared clock ticks.
func SecondsToClock(sec int64) int64 { return sec * Rate }
