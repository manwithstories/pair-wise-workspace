// Package clock provides integer-only timebase conversions shared by the
// parser, track mapper, remuxer and verifier.
//
// Matroska timestamps are expressed in ticks whose length is given by the
// Segment TimestampScale (nanoseconds per tick). MP4 uses per-track timescales.
// Every conversion in this project goes through this package so that repeated
// runs over the same input produce byte-identical mappings and digests.
package clock

import (
	"errors"
	"fmt"
	"math"
)

const (
	// NanosPerSecond is the number of nanoseconds in one second.
	NanosPerSecond int64 = 1_000_000_000

	// DefaultTimescale is the MP4 track timescale used for every output track.
	// Microseconds keep millisecond Matroska timestamps exact.
	DefaultTimescale int64 = 1_000_000

	// DefaultTimestampScale is the Matroska default: one millisecond.
	DefaultTimestampScale int64 = 1_000_000
)

var (
	// ErrInvalidTimescale is returned for non-positive timescales.
	ErrInvalidTimescale = errors.New("clock: timescale must be positive")
	// ErrOverflow is returned when a conversion does not fit in int64.
	ErrOverflow = errors.New("clock: integer overflow")
)

// Timebase converts Matroska ticks to nanoseconds.
type Timebase struct {
	// TimestampScale is the number of nanoseconds represented by one tick.
	TimestampScale int64
}

// NewTimebase validates and returns a Timebase.
func NewTimebase(timestampScale int64) (Timebase, error) {
	if timestampScale <= 0 {
		return Timebase{}, fmt.Errorf("%w: %d", ErrInvalidTimescale, timestampScale)
	}
	return Timebase{TimestampScale: timestampScale}, nil
}

// MustTimebase is NewTimebase for constants and tests.
func MustTimebase(timestampScale int64) Timebase {
	tb, err := NewTimebase(timestampScale)
	if err != nil {
		panic(err)
	}
	return tb
}

// Nanos converts ticks to nanoseconds, returning ErrOverflow instead of
// wrapping around.
func (tb Timebase) Nanos(ticks int64) (int64, error) {
	if tb.TimestampScale <= 0 {
		return 0, ErrInvalidTimescale
	}
	if ticks > math.MaxInt64/tb.TimestampScale || ticks < math.MinInt64/tb.TimestampScale {
		return 0, ErrOverflow
	}
	return ticks * tb.TimestampScale, nil
}

// RoundDiv divides num by den rounding half away from zero. den must be > 0.
func RoundDiv(num, den int64) int64 {
	if den <= 0 {
		panic("clock: RoundDiv denominator must be positive")
	}
	if num >= 0 {
		return (num + den/2) / den
	}
	return -((-num + den/2) / den)
}

// NanosToTicks converts a nanosecond value to a timescale with rounding half
// away from zero.
func NanosToTicks(nanos, timescale int64) int64 {
	return RoundDiv(nanos, NanosPerSecond/timescale)
}

// TicksToTimescale converts source ticks directly to an output timescale.
func (tb Timebase) TicksToTimescale(ticks, timescale int64) (int64, error) {
	if timescale <= 0 {
		return 0, ErrInvalidTimescale
	}
	nanos, err := tb.Nanos(ticks)
	if err != nil {
		return 0, err
	}
	return NanosToTicks(nanos, timescale), nil
}
