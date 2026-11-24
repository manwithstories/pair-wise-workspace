// Package clock defines the single integer time base used by every
// subsystem. All track timestamps are converted to ticks at TickRate so
// that events coming from different timescales can be ordered exactly.
package clock

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
)

// TickRate is the unified tick rate (90 kHz, the MPEG system clock).
const TickRate Tick = 90000

// Tick is an integer position on the unified timeline.
type Tick int64

var (
	ErrTimescale       = errors.New("clock: timescale must be positive")
	ErrOverflow        = errors.New("clock: arithmetic overflow")
	ErrDuration        = errors.New("clock: duration must not be negative")
	ErrBadRollbackTick = errors.New("clock: timestamp regression too large")
)

// Add returns a+b, rejecting signed overflow.
func Add(a, b Tick) (Tick, error) {
	if b > 0 && a > Tick(math.MaxInt64)-b {
		return 0, fmt.Errorf("%w: %d + %d", ErrOverflow, a, b)
	}
	if b < 0 && a < Tick(math.MinInt64)-b {
		return 0, fmt.Errorf("%w: %d + %d", ErrOverflow, a, b)
	}
	return a + b, nil
}

// Sub returns a-b, rejecting signed overflow.
func Sub(a, b Tick) (Tick, error) {
	if b == Tick(math.MinInt64) {
		if a >= 0 {
			return 0, fmt.Errorf("%w: %d - %d", ErrOverflow, a, b)
		}
		return a - b, nil
	}
	return Add(a, -b)
}

// ScaleToTicks converts a timestamp expressed at timescale units/second to
// the unified 90 kHz base. The result is floored. Negative values are
// supported but the magnitude must not overflow.
func ScaleToTicks(value int64, timescale uint32) (Tick, error) {
	if timescale == 0 {
		return 0, ErrTimescale
	}
	if value == 0 {
		return 0, nil
	}
	neg := value < 0
	var magnitude uint64
	if neg {
		magnitude = uint64(-(value + 1)) + 1
	} else {
		magnitude = uint64(value)
	}
	hi, lo := bits.Mul64(magnitude, uint64(TickRate))
	div := uint64(timescale)
	if hi >= div {
		return 0, fmt.Errorf("%w: %d * %d / %d", ErrOverflow, value, TickRate, timescale)
	}
	q, _ := bits.Div64(hi, lo, div)
	if q > math.MaxInt64 {
		return 0, fmt.Errorf("%w: result %d", ErrOverflow, q)
	}
	result := Tick(q)
	if neg {
		result = -result
	}
	return result, nil
}

// TicksToUnits is the inverse conversion used when writing boxes back to a
// track timescale. It is floored in the same way as ScaleToTicks.
func TicksToUnits(t Tick, timescale uint32) (int64, error) {
	if timescale == 0 {
		return 0, ErrTimescale
	}
	if t == 0 {
		return 0, nil
	}
	neg := t < 0
	var magnitude uint64
	if neg {
		magnitude = uint64(-(t + 1)) + 1
	} else {
		magnitude = uint64(t)
	}
	hi, lo := bits.Mul64(magnitude, uint64(timescale))
	div := uint64(TickRate)
	if hi >= div {
		return 0, fmt.Errorf("%w: %d * %d / %d", ErrOverflow, t, timescale, TickRate)
	}
	q, _ := bits.Div64(hi, lo, div)
	if q > math.MaxInt64 {
		return 0, fmt.Errorf("%w: result %d", ErrOverflow, q)
	}
	result := int64(q)
	if neg {
		result = -result
	}
	return result, nil
}

// ValidateDuration enforces a non-negative duration on the unified base.
func ValidateDuration(d Tick) error {
	if d < 0 {
		return fmt.Errorf("%w: %d", ErrDuration, d)
	}
	return nil
}
