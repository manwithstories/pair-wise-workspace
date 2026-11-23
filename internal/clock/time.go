// Package clock defines the shared rational time base used across probe,
// transport and engine packages. All media timestamps are converted to a
// common nanosecond rational representation and every conversion is checked
// for overflow.
package clock

import (
	"errors"
	"math"
)

// NanoPerSec is the numerator of the canonical time base: 1e9 nanoseconds.
const NanoPerSec int64 = 1_000_000_000

// Rational expresses Num/Den time units per second, e.g. 90000/1 for an
// RTP 90 kHz clock or 24000/1001 for a video track.
type Rational struct {
	Num int64
	Den int64
}

// Valid reports whether the time base itself is usable: zero or negative
// denominators/numerators are rejected as invalid track time bases.
func (r Rational) Valid() bool {
	return r.Num > 0 && r.Den > 0
}

// mulOverflows reports whether a*b would overflow an int64.
func mulOverflows(a, b int64) bool {
	if a == 0 || b == 0 {
		return false
	}
	if a == math.MinInt64 || b == math.MinInt64 {
		return true
	}
	av, bv := a, b
	if av < 0 {
		av = -av
	}
	if bv < 0 {
		bv = -bv
	}
	return av > math.MaxInt64/bv
}

// ToNanos converts a tick count in this time base to whole nanoseconds.
// It returns an error when the time base is invalid or the conversion
// overflows int64.
func (r Rational) ToNanos(ticks int64) (int64, error) {
	if !r.Valid() {
		return 0, errors.New("clock: invalid time base")
	}
	if mulOverflows(ticks, NanoPerSec) {
		return 0, errors.New("clock: timestamp overflow in nanosecond conversion")
	}
	return ticks * NanoPerSec * r.Den / r.Num, nil
}

// FromNanos converts whole nanoseconds into a tick count in this time base,
// rounding to the nearest tick. Overflow is reported rather than wrapped.
func (r Rational) FromNanos(nanos int64) (int64, error) {
	if !r.Valid() {
		return 0, errors.New("clock: invalid time base")
	}
	if mulOverflows(nanos, r.Num) {
		return 0, errors.New("clock: timestamp overflow in tick conversion")
	}
	num := nanos * r.Num
	den := NanoPerSec * r.Den
	if num >= 0 {
		return (num + den/2) / den, nil
	}
	return -((-num + den/2) / den), nil
}

// AddNanos adds a nanosecond delta to a tick timestamp and checks overflow.
func (r Rational) AddNanos(ticks, nanos int64) (int64, error) {
	delta, err := r.FromNanos(nanos)
	if err != nil {
		return 0, err
	}
	if (delta > 0 && ticks > math.MaxInt64-delta) ||
		(delta < 0 && ticks < math.MinInt64-delta) {
		return 0, errors.New("clock: timestamp addition overflow")
	}
	return ticks + delta, nil
}

// TimeRange is a half-open interval [Start, End) in nanoseconds on the
// shared time base. End == Start denotes an empty range.
type TimeRange struct {
	StartNanos int64
	EndNanos   int64
}

// Valid reports whether the range is well-formed and non-empty.
func (tr TimeRange) Valid() bool {
	return tr.StartNanos >= 0 && tr.EndNanos >= tr.StartNanos
}

// DurationNanos returns the range length in nanoseconds.
func (tr TimeRange) DurationNanos() int64 {
	return tr.EndNanos - tr.StartNanos
}

// Overlaps reports whether two half-open ranges intersect.
func (tr TimeRange) Overlaps(other TimeRange) bool {
	return tr.StartNanos < other.EndNanos && other.StartNanos < tr.EndNanos
}
