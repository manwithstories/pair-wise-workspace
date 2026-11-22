// Package clock defines the unified rational timebase used by every ingest,
// segmentation and packaging component. All timeline positions are reduced to
// nanosecond ticks on a single int64 axis; conversions validate the source
// clock and report track/raw-timestamp/position context without ever exposing
// media content in error strings.
package clock

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/bits"
	"time"
)

// Rate is a rational clock rate expressed as Num/Den ticks per second.
// A 90 kHz RTP clock is Rate{90000, 1}; an MP4 timescale of 48000 is the
// same shape. The zero value is invalid: zero clocks are rejected.
type Rate struct {
	Num int64 // ticks ...
	Den int64 // ... per Den seconds
}

// Nano is the canonical clock: one tick per nanosecond.
var Nano = Rate{Num: 1_000_000_000, Den: 1}

// Time is a position on the unified timeline, stored as nanoseconds.
type Time int64

// Range is a half-open [Start, End) interval on the unified timeline.
type Range struct {
	Start Time
	End   Time
}

// Error carries locating information for a failed timebase operation. It only
// contains track id, raw timestamp and stream offset — never sample payload.
type Error struct {
	Op      string
	TrackID uint32
	RawTS   int64
	Offset  int64
	Err     error
}

func (e *Error) Error() string {
	return fmt.Sprintf("clock: %s failed: track=%d raw_ts=%d offset=%d: %s",
		e.Op, e.TrackID, e.RawTS, e.Offset, e.Err.Error())
}

// Unwrap exposes the sentinel cause (ErrZeroClock/ErrNegative/ErrOverflow).
func (e *Error) Unwrap() error { return e.Err }

// Sentinel reasons wrapped inside *Error.
var (
	ErrZeroClock = errors.New("zero or non-positive clock rate")
	ErrNegative  = errors.New("negative duration or timestamp")
	ErrOverflow  = errors.New("timestamp conversion overflow")
)

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// NewRate validates and normalizes a rational clock rate.
func NewRate(num, den int64) (Rate, error) {
	r := Rate{Num: num, Den: den}
	if num <= 0 || den <= 0 {
		return Rate{}, fmt.Errorf("%w: num=%d den=%d", ErrZeroClock, num, den)
	}
	g := gcd(num, den)
	r.Num /= g
	r.Den /= g
	return r, nil
}

// Valid reports whether r is a usable clock.
func (r Rate) Valid() bool { return r.Num > 0 && r.Den > 0 }

// TicksPerSecond returns the normalized rate fraction.
func (r Rate) TicksPerSecond() (num, den int64) {
	g := gcd(r.Num, r.Den)
	return r.Num / g, r.Den / g
}

// mul64 returns a*b and an overflow flag against the signed int64 range.
func mul64(a, b uint64) (uint64, bool) {
	hi, lo := bits.Mul64(a, b)
	if hi != 0 || lo > math.MaxInt64 {
		return 0, true
	}
	return lo, false
}

// toNanos converts a non-negative tick count in rate r to canonical
// nanoseconds, rejecting overflow of the signed 64-bit timeline.
func (r Rate) toNanos(ticks int64) (int64, error) {
	if !r.Valid() {
		return 0, ErrZeroClock
	}
	if ticks < 0 {
		return 0, ErrNegative
	}
	p, overflow := mul64(uint64(ticks), uint64(r.Den))
	if overflow {
		return 0, ErrOverflow
	}
	p, overflow = mul64(p, 1_000_000_000)
	if overflow {
		return 0, ErrOverflow
	}
	return int64(p / uint64(r.Num)), nil
}

// ToTime converts a raw timestamp to unified time.
func (r Rate) ToTime(ticks int64) (Time, error) {
	ns, err := r.toNanos(ticks)
	if err != nil {
		return 0, err
	}
	return Time(ns), nil
}

// ToTimeLocated is ToTime with full locating context for error reports.
func (r Rate) ToTimeLocated(ticks int64, trackID uint32, offset int64) (Time, *Error) {
	ns, err := r.toNanos(ticks)
	if err != nil {
		return 0, &Error{Op: "ToTime", TrackID: trackID, RawTS: ticks, Offset: offset, Err: err}
	}
	return Time(ns), nil
}

// Duration converts a non-negative tick delta to a time.Duration, rejecting
// negative durations and overflow.
func (r Rate) Duration(deltaTicks int64) (time.Duration, error) {
	if deltaTicks < 0 {
		return 0, ErrNegative
	}
	ns, err := r.toNanos(deltaTicks)
	if err != nil {
		return 0, err
	}
	return time.Duration(ns), nil
}

// FromTime converts canonical nanoseconds back into ticks of rate r, rounding
// to the nearest tick. It is used when emitting manifests in a track clock.
func (r Rate) FromTime(t Time) (int64, error) {
	if !r.Valid() {
		return 0, ErrZeroClock
	}
	if t < 0 {
		return 0, ErrNegative
	}
	p, overflow := mul64(uint64(t), uint64(r.Num))
	if overflow {
		return 0, ErrOverflow
	}
	return int64((p + uint64(r.Den)*1_000_000_000/2) / (uint64(r.Den) * 1_000_000_000)), nil
}

// Nanoseconds exposes the raw int64 axis value.
func (t Time) Nanoseconds() int64 { return int64(t) }

// Millis is the millisecond position, rounded to nearest.
func (t Time) Millis() int64 { return int64((int64(t) + 500_000) / 1_000_000) }

// Std returns the value as a time.Duration.
func (t Time) Std() time.Duration { return time.Duration(t) }

// Add advances the timeline position, rejecting overflow.
func (t Time) Add(d time.Duration) (Time, error) {
	s := int64(t) + int64(d)
	if (d > 0 && s < int64(t)) || (d < 0 && s > int64(t)) {
		return 0, ErrOverflow
	}
	return Time(s), nil
}

// Contains reports whether t falls in the half-open interval [Start, End).
// An event exactly on End belongs to the following segment, never this one.
func (g Range) Contains(t Time) bool { return t >= g.Start && t < g.End }

// Overlaps reports whether two half-open ranges share any instant.
func (g Range) Overlaps(o Range) bool { return g.Start < o.End && o.Start < g.End }

// Duration is the interval length; a malformed inverted range yields an error.
func (g Range) Duration() (time.Duration, error) {
	d := int64(g.End) - int64(g.Start)
	if d < 0 {
		return 0, ErrNegative
	}
	return time.Duration(d), nil
}

// CmpTicks compares two timestamps carried on different rational clocks
// exactly, via cross multiplication. It returns -1/0/1. Overflow of the
// cross product falls back to math/big rather than guessing.
func CmpTicks(a int64, ra Rate, b int64, rb Rate) (int, error) {
	if !ra.Valid() || !rb.Valid() {
		return 0, ErrZeroClock
	}
	// Fast path: the full cross products fit in 64 bits.
	if bits.Len64(uint64(a))+bits.Len64(uint64(ra.Den))+bits.Len64(uint64(rb.Num)) <= 64 &&
		bits.Len64(uint64(b))+bits.Len64(uint64(rb.Den))+bits.Len64(uint64(ra.Num)) <= 64 {
		x := uint64(a) * uint64(ra.Den) * uint64(rb.Num)
		y := uint64(b) * uint64(rb.Den) * uint64(ra.Num)
		switch {
		case x < y:
			return -1, nil
		case x > y:
			return 1, nil
		default:
			return 0, nil
		}
	}
	ba := big.NewInt(a)
	ba.Mul(ba, big.NewInt(ra.Den)).Mul(ba, big.NewInt(rb.Num))
	bb := big.NewInt(b)
	bb.Mul(bb, big.NewInt(rb.Den)).Mul(bb, big.NewInt(ra.Num))
	return ba.Cmp(bb), nil
}
