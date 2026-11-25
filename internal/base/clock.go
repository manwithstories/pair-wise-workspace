// Package base defines the shared 90 kHz media clock and the structured
// fault type used across ingest, audio, engine and verify subsystems.
package base

// ClockRate is the single media clock frequency shared by every rendition
// and by the audio track, as required for cross-rendition alignment.
const ClockRate uint64 = 90000

// WrapPeriod is the 33-bit RTP timestamp modulus used by bare streams.
const WrapPeriod uint64 = 1 << 33

// MaxFileBytes limits a single input file to 1 GiB.
const MaxFileBytes int64 = 1 << 30

// MaxAccessUnitBytes limits one access unit to 16 MiB.
const MaxAccessUnitBytes int = 1 << 24

// Clock converts raw, possibly wrapping or backwards, encoder timestamps
// into a strictly monotonic 90 kHz presentation timeline. The zero value is
// not ready; construct one with NewClock.
type Clock struct {
	rate         uint64
	wrapPeriod   uint64
	initialized  bool
	lastRaw      uint64
	lastOut      uint64
	wraps        uint64
	restarts     uint64
	gaps         uint64
	// MaxBackStep is the largest negative delta still treated as an
	// in-stream jitter step rather than an encoder restart, in ticks.
	MaxBackStep uint64
	// MaxForwardStep is the largest positive delta accepted as continuous.
	// Larger steps open a gap instead of being bridged silently.
	MaxForwardStep uint64
}

// ClockSnapshot reports the current clock state for diagnostics.
type ClockSnapshot struct {
	OutputPTS    uint64
	WrapCount    uint64
	RestartCount uint64
	GapCount     uint64
}

// NewClock builds a clock with the shared 90 kHz rate.
func NewClock() *Clock {
	return &Clock{
		rate:           ClockRate,
		wrapPeriod:     WrapPeriod,
		MaxBackStep:    ClockRate / 2,
		MaxForwardStep: ClockRate * 60,
	}
}

// Advance feeds one raw timestamp and returns the normalized output PTS plus
// a gap flag. A true gap means the jump could not be resolved unambiguously:
// the output PTS is still strictly increasing but callers must not splice
// across the boundary. Encoder restarts are handled by re-basing the raw
// clock onto the current output position.
func (c *Clock) Advance(raw uint64) (out uint64, gap bool) {
	// RTP-style media timestamps live in a 33-bit modular field regardless
	// of the source's native width.
	raw %= c.wrapPeriod
	if !c.initialized {
		c.initialized = true
		c.lastRaw = raw
		c.lastOut = 0
		return 0, false
	}

	// Unsigned modular distance on the raw field. forward is the clockwise
	// distance; a value over half the modulus means the counter rolled under.
	forward := raw - c.lastRaw
	backward := c.lastRaw - raw
	half := c.wrapPeriod / 2

	switch {
	case forward == 0:
		// Duplicate timestamp: keep strict monotonicity by one tick.
		c.lastOut++
	case forward < half:
		// Ordinary forward step within the field.
		if forward > c.MaxForwardStep {
			c.gaps++
			gap = true
			c.lastOut++
		} else {
			c.lastOut += forward
		}
	case backward <= c.MaxBackStep:
		// Small backwards jitter: preserve monotonic order by one tick.
		c.lastOut++
	case backward < half:
		// Large genuine backwards jump without a wrap: encoder restart.
		c.restarts++
		c.lastOut++
	default:
		// Rollunder: the real elapsed span is the clockwise remainder.
		c.wraps++
		span := c.wrapPeriod - backward
		if span > c.MaxForwardStep {
			c.gaps++
			gap = true
			c.lastOut++
		} else {
			c.lastOut += span
		}
	}

	c.lastRaw = raw
	out = c.lastOut
	return out, gap
}

// Snapshot returns a diagnostic copy of the clock state.
func (c *Clock) Snapshot() ClockSnapshot {
	return ClockSnapshot{
		OutputPTS:    c.lastOut,
		WrapCount:    c.wraps,
		RestartCount: c.restarts,
		GapCount:     c.gaps,
	}
}

// Seconds converts a 90 kHz tick count to seconds.
func Seconds(ticks uint64) float64 {
	return float64(ticks) / float64(ClockRate)
}
