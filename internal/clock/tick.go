// Package clock defines the single time base shared by every cinepipe
// subsystem. MPEG-TS timestamps (PCR, PTS, DTS) are 33-bit counters that
// advance at 90 kHz and wrap roughly every 26.5 hours. Unwrapper converts a
// wrapping counter into a monotonic absolute tick count so that the scanner
// and the deliverer can compare timestamps from different clock domains on
// one timeline.
package clock

import (
	"fmt"
	"math"
	"strconv"
)

// Base is the MPEG clock frequency: 90 kHz.
const Base int64 = 90000

const (
	// PCRModulus is the wrap period of the 33-bit PCR base counter.
	PCRModulus uint64 = 1 << 33
	// PTSModulus is the wrap period of the 33-bit PTS/DTS counter.
	PTSModulus uint64 = 1 << 33
	// PCRWrapHours is the wall-clock wrap period, for documentation only.
	PCRWrapHours = float64(PCRModulus) / float64(Base) / 3600
)

// NoTime marks an unknown or not-yet-established timestamp. It is deliberately
// outside the range any valid stream can reach (2^33 ticks is under one day).
const NoTime int64 = math.MinInt64

// Millis converts 90 kHz ticks to milliseconds.
func Millis(ticks int64) float64 {
	return float64(ticks) * 1000 / float64(Base)
}

// Us converts 90 kHz ticks to microseconds.
func Us(ticks int64) float64 {
	return float64(ticks) * 1e6 / float64(Base)
}

// TicksForMillis converts milliseconds to 90 kHz ticks, rounding to nearest.
func TicksForMillis(ms int64) int64 {
	return ms * Base / 1000
}

// TicksForSeconds converts seconds to 90 kHz ticks.
func TicksForSeconds(s int64) int64 {
	return s * Base
}

// FormatMS renders ticks as milliseconds with three decimals. Fixed width and
// no locale effects keep report and manifest output byte-identical across runs.
func FormatMS(ticks int64) string {
	if ticks == NoTime {
		return "n/a"
	}
	neg := ticks < 0
	us := ticks * 1000 / 90 // signed microseconds, truncating toward zero
	if neg {
		us = -us
	}
	s := strconv.FormatInt(us/1000, 10) + "." + fmt.Sprintf("%03d", us%1000)
	if neg {
		return "-" + s
	}
	return s
}

// Unwrapper reconstructs a monotonic absolute counter from a wrapping one.
//
// The wrap decision uses a half-modulus window: a delta that would need a jump
// larger than half the modulus is interpreted as the counter crossing the wrap
// boundary (or crossing back). MPEG counters advance far more slowly than half
// a modulus per real timestamp, so this is unambiguous.
type Unwrapper struct {
	modulus uint64
	last    uint64
	offset  int64
	started bool
	adjusts int64
}

// NewUnwrapper returns an unwrapper for the given counter modulus. Use
// PCRModulus or PTSModulus.
func NewUnwrapper(modulus uint64) *Unwrapper {
	return &Unwrapper{modulus: modulus}
}

// Unwrap returns the absolute tick value and whether the modulus boundary was
// crossed on this sample. The first sample establishes the floor and never
// reports a wrap.
func (u *Unwrapper) Unwrap(raw uint64) (int64, bool) {
	if u.modulus == 0 {
		return int64(raw), false
	}
	raw %= u.modulus
	if !u.started {
		u.started = true
		u.last = raw
		return int64(raw) + u.offset, false
	}
	delta := int64(raw) - int64(u.last)
	half := int64(u.modulus / 2)
	wrapped := false
	switch {
	case delta < -half:
		u.offset += int64(u.modulus)
		u.adjusts++
		wrapped = true
	case delta > half:
		u.offset -= int64(u.modulus)
		u.adjusts--
		wrapped = true
	}
	u.last = raw
	return int64(raw) + u.offset, wrapped
}

// Adjusts reports how many modulus boundaries the unwrapper has crossed. A
// positive value means forward wraps; negative means backward reseeks.
func (u *Unwrapper) Adjusts() int64 { return u.adjusts }

// Last returns the most recent raw counter value.
func (u *Unwrapper) Last() uint64 { return u.last }

// Started reports whether any sample has been seen.
func (u *Unwrapper) Started() bool { return u.started }

// Rel expresses an absolute tick on a timeline whose origin is given. Callers
// pass the origin captured from the first timestamp of the stream.
func Rel(origin, absolute int64) int64 {
	if origin == NoTime || absolute == NoTime {
		return NoTime
	}
	return absolute - origin
}

// MaxRel returns the larger relative timestamp, ignoring NoTime.
func MaxRel(a, b int64) int64 {
	if a == NoTime {
		return b
	}
	if b == NoTime {
		return a
	}
	if a > b {
		return a
	}
	return b
}
