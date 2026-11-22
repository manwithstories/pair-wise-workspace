package clock

import (
	"math"

	"github.com/abema/go-mp4"
)

// Timebase is the shared nanosecond base for every track. Matroska stores
// timecodes as integer ticks whose unit is the TimestampScale element
// (nanoseconds per tick, default 1_000_000 i.e. 1ms). All timeline and
// manifest math is performed in nanoseconds after conversion here.
//
// The mp4.Timescale concept (units per second, used by go-mp4's
// Mdhd/Tkhd boxes) is the inverse of our nanosecond scale; Mp4Scale
// bridges the two container families onto one common axis.
type Timebase struct {
	// ScaleNS is the duration in nanoseconds of one Matroska tick.
	ScaleNS int64
	// ToleranceNS is the maximum tolerated distance when aligning
	// boundaries coming from different representations/tracks.
	ToleranceNS int64
}

// DefaultTimebase returns the Matroska default: 1ms ticks, 5ms tolerance.
func DefaultTimebase() *Timebase {
	return &Timebase{ScaleNS: 1_000_000, ToleranceNS: 5_000_000}
}

// Mp4Scale converts an MP4-style timescale (ticks per second, as carried
// in go-mp4's Mdhd box) into the equivalent nanosecond scale.
func Mp4Scale(mdhd mp4.Mdhd) (int64, error) {
	if mdhd.Timescale == 0 {
		return 0, NewError(CatOverflow, "MP4 timescale must not be zero")
	}
	return int64(1_000_000_000) / int64(mdhd.Timescale), nil
}

// ToNanos converts Matroska ticks to nanoseconds, rejecting overflow.
func (t *Timebase) ToNanos(ticks int64) (int64, error) {
	if t.ScaleNS <= 0 {
		return 0, NewError(CatOverflow, "timebase scale must be positive, got %d", t.ScaleNS)
	}
	if ticks > 0 && t.ScaleNS > math.MaxInt64/ticks {
		return 0, NewError(CatOverflow, "tick*scale overflow: ticks=%d scale=%d", ticks, t.ScaleNS)
	}
	if ticks < 0 {
		// Go divides toward zero: MinInt64/ticks (ticks<0) is the
		// positive maximum scale whose product cannot underflow.
		if t.ScaleNS > math.MinInt64/ticks {
			return 0, NewError(CatOverflow, "negative tick*scale overflow: ticks=%d", ticks)
		}
	}
	return ticks * t.ScaleNS, nil
}

// FromNanos rounds nanoseconds down to whole Matroska ticks.
func (t *Timebase) FromNanos(ns int64) int64 {
	return ns / t.ScaleNS
}

// Align snaps a nanosecond timestamp down to the given common grid.
// gridNS must be positive.
func (t *Timebase) Align(ns, gridNS int64) (int64, error) {
	if gridNS <= 0 {
		return 0, NewError(CatOverflow, "alignment grid must be positive, got %d", gridNS)
	}
	if ns < 0 {
		// round toward negative infinity, not toward zero
		return ((ns - gridNS + 1) / gridNS) * gridNS, nil
	}
	return (ns / gridNS) * gridNS, nil
}

// WithinTolerance reports whether two nanosecond instants are no farther
// apart than the configured tolerance (absolute distance).
func (t *Timebase) WithinTolerance(a, b int64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= t.ToleranceNS
}

// WrapDiff returns the signed shortest modular distance (a-b) inside a
// ring of size mod with half-circle half. It is the core of the RTP
// timestamp/sequence boundary model: values within +/- half are treated
// as forward/backward steps, larger jumps are interpreted as wrap-around.
func WrapDiff(a, b, mod, half int64) int64 {
	d := (a - b) % mod
	if d < 0 {
		d += mod
	}
	if d > half {
		d -= mod
	}
	return d
}

// Unwrap lifts a modular value onto a monotonic int64 axis given the
// previous raw (modular) and unwrapped values. mod is the ring size
// (SeqMod for uint16 block timecodes/sequence numbers, TimeMod for
// uint32 RTP timestamps).
func Unwrap(prevRaw, prevUnwrapped, value, mod, half int64) int64 {
	return prevUnwrapped + WrapDiff(value, prevRaw, mod, half)
}

// Unwrap16 unwraps a uint16-width stream (Matroska block timecodes and
// RTP sequence numbers share this domain).
func Unwrap16(prevRaw, prevUnwrapped int64, value uint16) int64 {
	return Unwrap(prevRaw, prevUnwrapped, int64(value), SeqMod, HalfSeqMod)
}

// Unwrap32 unwraps a uint32-width stream (RTP timestamp domain).
func Unwrap32(prevRaw, prevUnwrapped int64, value uint32) int64 {
	return Unwrap(prevRaw, prevUnwrapped, int64(value), TimeMod, HalfTimeMod)
}
