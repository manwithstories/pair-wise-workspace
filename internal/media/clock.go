// Package media contains the shared media clock and error model used by the
// parser, timeline aligner, cache, checkpoint and manifest packages.
package media

import (
	"errors"
	"math"
	"math/bits"
)

// ClockRate is the common MPEG/HLS presentation time base: 90 kHz.
const ClockRate uint64 = 90_000

// ErrBadTimescale is returned when a track timescale is zero.
var ErrBadTimescale = errors.New("media timescale must be positive")

// Clock converts a track-local time base to the common 90 kHz clock.
type Clock struct {
	Timescale uint32
}

// NewClock validates and returns a track clock.
func NewClock(timescale uint32) (Clock, error) {
	if timescale == 0 {
		return Clock{}, ErrBadTimescale
	}
	return Clock{Timescale: timescale}, nil
}

// To90k converts value from the track timescale to 90 kHz, rounding to the
// nearest tick. Multiplication and conversion to signed time are checked.
func (c Clock) To90k(value uint64) (int64, error) {
	if c.Timescale == 0 {
		return 0, ErrBadTimescale
	}
	hi, lo := bits.Mul64(value, ClockRate)
	if hi != 0 {
		return 0, &Fault{Code: CodeIntegerOverflow, Message: "clock conversion product overflows uint64", Recoverable: false}
	}
	half := uint64(c.Timescale) / 2
	if lo > math.MaxUint64-half {
		return 0, &Fault{Code: CodeIntegerOverflow, Message: "clock conversion rounding overflows", Recoverable: false}
	}
	ticks := (lo + half) / uint64(c.Timescale)
	if ticks > math.MaxInt64 {
		return 0, &Fault{Code: CodeIntegerOverflow, Message: "90 kHz timestamp overflows int64", Recoverable: false}
	}
	return int64(ticks), nil
}

// Convert90k is the package-level helper for one-off conversions.
func Convert90k(value uint64, timescale uint32) (int64, error) {
	c, err := NewClock(timescale)
	if err != nil {
		return 0, err
	}
	return c.To90k(value)
}

// Seconds renders a 90 kHz tick count as floating-point seconds.
func Seconds(ticks int64) float64 {
	return float64(ticks) / float64(ClockRate)
}

// AbsTickDiff returns the absolute tick difference. Calling with extreme
// values is safe because the subtraction is performed without overflow.
func AbsTickDiff(a, b int64) uint64 {
	if a >= b {
		return uint64(a - b)
	}
	return uint64(b - a)
}

// WithinOneTick reports whether two common-clock values differ by at most one
// 90 kHz tick, the alignment tolerance allowed by the acceptance criteria.
func WithinOneTick(a, b int64) bool {
	return AbsTickDiff(a, b) <= 1
}

// CommonBoundary returns index*duration on the common clock.
func CommonBoundary(index uint64, duration90k int64) (int64, error) {
	if duration90k <= 0 {
		return 0, &Fault{Code: CodeInvalidArgument, Message: "common boundary duration must be positive", Recoverable: false}
	}
	hi, lo := bits.Mul64(index, uint64(duration90k))
	if hi != 0 || lo > math.MaxInt64 {
		return 0, &Fault{Code: CodeIntegerOverflow, Message: "common boundary overflows signed ticks", Recoverable: false}
	}
	return int64(lo), nil
}
