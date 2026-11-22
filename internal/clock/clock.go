// Package clock defines the single media time base used by every layer.
//
// Every track keeps its native timescale (ticks per second). The package
// converts between ticks and the shared millisecond timeline with one
// rounding rule: half away from zero, applied once per conversion. Callers
// must validate a timescale before use; conversions with a zero timescale
// return zero rather than panicking so damaged input can still be reported.
package clock

import (
	"errors"
	"math"
)

const (
	// Milliseconds is the shared presentation timeline unit.
	Milliseconds = 1000
	// Microseconds is used for sub-millisecond diagnostics.
	Microseconds = 1_000_000
	// DefaultToleranceMS is the maximum allowed boundary disagreement
	// between representations sharing the same clock.
	DefaultToleranceMS int64 = 1
	// DefaultSegmentMS is the reference segment duration for the manifest.
	DefaultSegmentMS int64 = 2000
)

// ErrInvalidTimescale is returned when a track declares a zero timescale.
var ErrInvalidTimescale = errors.New("clock: timescale must be positive")

// Base is a validated media timescale.
type Base struct {
	Timescale uint32
}

// NewBase validates and wraps a track timescale.
func NewBase(timescale uint32) (Base, error) {
	if timescale == 0 {
		return Base{}, ErrInvalidTimescale
	}
	return Base{Timescale: timescale}, nil
}

// Valid reports whether the base can perform conversions.
func (b Base) Valid() bool { return b.Timescale != 0 }

// DurationMS converts ticks to the shared timeline.
func (b Base) DurationMS(ticks uint64) int64 {
	return TicksToMS(ticks, b.Timescale)
}

// DurationMicros converts ticks to microseconds.
func (b Base) DurationMicros(ticks uint64) int64 {
	return TicksToMicros(ticks, b.Timescale)
}

// Ticks converts a shared-timeline duration to native ticks.
func (b Base) Ticks(ms int64) uint64 {
	return MSToTicks(ms, b.Timescale)
}

// TicksToMS converts ticks to milliseconds using round-half-up. It saturates
// instead of overflowing so corrupt durations cannot wrap into negative time.
func TicksToMS(ticks uint64, timescale uint32) int64 {
	if timescale == 0 {
		return 0
	}
	ts := uint64(timescale)
	q, r := ticks/ts, ticks%ts
	if q > math.MaxInt64/Milliseconds {
		return math.MaxInt64
	}
	ms := q * Milliseconds
	frac := (r*Milliseconds + ts/2) / ts
	if ms > math.MaxInt64-frac {
		return math.MaxInt64
	}
	return int64(ms + frac)
}

// TicksToMicros converts ticks to microseconds with the same rounding rule.
func TicksToMicros(ticks uint64, timescale uint32) int64 {
	if timescale == 0 {
		return 0
	}
	ts := uint64(timescale)
	q, r := ticks/ts, ticks%ts
	if q > math.MaxInt64/Microseconds {
		return math.MaxInt64
	}
	us := q * Microseconds
	frac := (r*Microseconds + ts/2) / ts
	if us > math.MaxInt64-frac {
		return math.MaxInt64
	}
	return int64(us + frac)
}

// MSToTicks converts a non-negative millisecond duration to ticks. Negative
// input (used only by edit lists) is treated as zero; callers handle edits
// explicitly. Results saturate.
func MSToTicks(ms int64, timescale uint32) uint64 {
	if ms <= 0 || timescale == 0 {
		return 0
	}
	ts := uint64(timescale)
	q, r := uint64(ms)/Milliseconds, uint64(ms)%Milliseconds
	if q > math.MaxUint64/ts {
		return math.MaxUint64
	}
	ticks := q * ts
	frac := (r*ts + Milliseconds/2) / Milliseconds
	if ticks > math.MaxUint64-frac {
		return math.MaxUint64
	}
	return ticks + frac
}

// SegmentIndex maps a presentation timestamp to its segment bucket. Times
// before the origin and non-positive segment lengths map to bucket zero.
func SegmentIndex(ms, segmentMS int64) int {
	if segmentMS <= 0 || ms <= 0 {
		return 0
	}
	idx := ms / segmentMS
	if idx > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(idx)
}

// SegmentStart returns the nominal shared-clock start of a bucket.
func SegmentStart(index int, segmentMS int64) int64 {
	if index <= 0 || segmentMS <= 0 {
		return 0
	}
	if int64(index) > math.MaxInt64/segmentMS {
		return math.MaxInt64
	}
	return int64(index) * segmentMS
}

// WithinTolerance reports whether two timestamps agree within tol.
func WithinTolerance(a, b, tol int64) bool {
	return AbsDiff(a, b) <= tol
}

// AbsDiff returns |a-b| without overflowing on extreme inputs.
func AbsDiff(a, b int64) int64 {
	if a >= b {
		if a > 0 && b < 0 && a > math.MaxInt64+b {
			return math.MaxInt64
		}
		return a - b
	}
	return AbsDiff(b, a)
}

// AddMS adds two shared-timeline offsets, saturating on overflow.
func AddMS(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64
	}
	return a + b
}

// RTPTimestamp maps a shared-timeline timestamp to a 32-bit RTP timestamp at
// the given clock rate. Arithmetic is performed modulo 2^32 so wraparound is
// preserved exactly, as required by RTP receivers.
func RTPTimestamp(ms int64, clockRate uint32) uint32 {
	if ms <= 0 || clockRate == 0 {
		return 0
	}
	q := uint64(ms) / Milliseconds
	r := uint64(ms) % Milliseconds
	// (q*rate) mod 2^32. The operands are masked before multiplying so the
	// product fits in 64 bits; only the low 32 bits are retained.
	lowQ := q & 0xffffffff
	ticks := uint32(lowQ * uint64(clockRate))
	frac := uint32((r*uint64(clockRate) + Milliseconds/2) / Milliseconds)
	return ticks + frac // deliberate uint32 wrap
}

// RTPDeltaMS converts a wrap-safe RTP timestamp delta back to milliseconds.
func RTPDeltaMS(delta uint32, clockRate uint32) int64 {
	if clockRate == 0 {
		return 0
	}
	return int64((uint64(delta)*Milliseconds + uint64(clockRate)/2) / uint64(clockRate))
}

// RTPDiff returns a-b as a signed wrap-safe difference.
func RTPDiff(a, b uint32) int32 {
	return int32(a - b)
}
