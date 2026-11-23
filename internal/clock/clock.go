// Package clock provides the unified media time base shared by every
// pipeline module, plus wrap-around extension for RTP sequence numbers
// and timestamps.
package clock

import "time"

// Rate is the unified time base: 90 kHz, the RTP standard media clock.
const Rate = 90000

// Time is an instant or interval on the unified media time line,
// expressed in 1/Rate second units.
type Time int64

// FromTicks converts absolute codec-clock ticks to the unified time base.
func FromTicks(ticks uint64, codecRate uint32) Time {
	if codecRate == 0 {
		return 0
	}
	// Round to nearest to bound the cumulative error of per-packet
	// conversions to half a unified tick.
	return Time((ticks*Rate + uint64(codecRate)/2) / uint64(codecRate))
}

// TicksToDuration converts a duration in codec-clock ticks to unified units.
func TicksToDuration(ticks uint64, codecRate uint32) Time {
	return FromTicks(ticks, codecRate)
}

// FromDuration converts a wall-clock duration to unified units.
func FromDuration(d time.Duration) Time {
	return Time(d.Nanoseconds() * Rate / int64(time.Second))
}

// Duration converts unified units to a wall-clock duration.
func (t Time) Duration() time.Duration {
	return time.Duration(int64(t) * int64(time.Second) / Rate)
}

// Millis returns t in fractional milliseconds.
func (t Time) Millis() float64 {
	return float64(t) * 1000 / Rate
}

// Unwrap16 extends a 16-bit RTP sequence number to 64 bits given the
// highest extended value seen so far, tolerating reordering within half
// a cycle.
func Unwrap16(prev uint64, cur uint16) uint64 {
	return unwrap(prev, uint64(cur), 16)
}

// Unwrap32 extends a 32-bit RTP timestamp to 64 bits likewise.
func Unwrap32(prev uint64, cur uint32) uint64 {
	return unwrap(prev, uint64(cur), 32)
}

func unwrap(prev, cur uint64, bits uint) uint64 {
	mod := uint64(1) << bits
	half := mod >> 1
	mask := mod - 1
	c := (prev &^ mask) | cur
	switch {
	case c+half < prev:
		// Forward wrap: cur belongs to the next cycle.
		c += mod
	case prev+half < c:
		// Backward across the boundary (reordered straggler). Near the
		// stream start the previous cycle does not exist; keep c as is
		// rather than underflowing.
		if c < mod {
			return c
		}
		c -= mod
	}
	return c
}
