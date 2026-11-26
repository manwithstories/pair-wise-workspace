// Package media defines the shared media time base, track time conversion
// and the unified fault model used by every subsystem of the relay pipeline.
package media

import "math"

// Common time base used on the repaired global timeline. All repaired DTS
// values are expressed in this timescale so that different tracks share one
// comparable clock while their original per-track clocks stay reversible.
const GlobalTimescale uint32 = 90000

// MaxTime is the largest representable media time unit. Any conversion that
// reaches or exceeds this value is treated as an overflow and the owning
// track must be isolated instead of being published.
const MaxTime uint64 = math.MaxUint64

// Clock describes one track's time base. Timescale is the number of time
// units that pass in one second, per ISO/IEC 14496-12 mdhd.timescale.
type Clock struct {
	TrackID    uint32
	Timescale uint32
}

// ToGlobal converts a time unit expressed on the track clock into a unit of
// the shared global clock. The second result is false when the conversion
// would overflow uint64; callers must isolate the track in that case.
func (c Clock) ToGlobal(trackUnits uint64) (uint64, bool) {
	if c.Timescale == 0 {
		return 0, false
	}
	// Split multiplication to detect overflow instead of silently wrapping.
	hi, lo := mul64(trackUnits, uint64(GlobalTimescale))
	if hi != 0 {
		return 0, false
	}
	return lo / uint64(c.Timescale), true
}

// FromGlobal converts a global time unit back into track time units,
// rounding to nearest. The second result is false on overflow.
func (c Clock) FromGlobal(globalUnits uint64) (uint64, bool) {
	if c.Timescale == 0 {
		return 0, false
	}
	hi, lo := mul64(globalUnits, uint64(c.Timescale))
	if hi != 0 {
		return 0, false
	}
	return lo / uint64(GlobalTimescale), true
}

// DurationGlobal converts a track-clock duration to global units.
func (c Clock) DurationGlobal(trackUnits uint64) (uint64, bool) {
	return c.ToGlobal(trackUnits)
}

// AddTime returns a+b and reports false when the sum overflows.
func AddTime(a, b uint64) (uint64, bool) {
	if a > MaxTime-b {
		return 0, false
	}
	return a + b, true
}

// mul64 returns the 128-bit product a*b as (high, low) 64-bit words.
func mul64(a, b uint64) (hi, lo uint64) {
	const mask32 = 1<<32 - 1
	aHi, aLo := a>>32, a&mask32
	bHi, bLo := b>>32, b&mask32
	lo = aLo * bLo
	mid1 := aHi * bLo
	mid2 := aLo * bHi
	carry := ((lo >> 32) + (mid1 & mask32) + (mid2 & mask32)) >> 32
	hi = aHi*bHi + (mid1 >> 32) + (mid2 >> 32) + carry
	return hi, lo
}
