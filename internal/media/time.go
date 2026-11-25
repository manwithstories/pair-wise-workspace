// Package media holds the shared, dependency-free primitives used by every
// core subsystem: an exact rational time base and the located fault model.
//
// All timestamp arithmetic is integer-only. No float ever touches a timeline
// value, so edit points and transition overlaps cannot accumulate drift.
package media

import (
	"errors"
	"math/big"
)

// Rational is an exact time base expressed as Num/Den ticks per second.
// Both fields are positive for a valid time base (e.g. 90000/1).
type Rational struct {
	Num int64
	Den int64
}

// Valid reports whether r is a usable, positive time base.
func (r Rational) Valid() bool { return r.Num > 0 && r.Den > 0 }

// Common time bases used by the generated fixtures and the RTP receiver.
var (
	TimeBaseVideo90k = Rational{Num: 90000, Den: 1}
	TimeBaseAudio48k = Rational{Num: 48000, Den: 1}
	TimeBaseMillis   = Rational{Num: 1000, Den: 1}
)

// Sentinel timeline errors. They are always wrapped in a Fault (see fault.go)
// before leaving a subsystem so the source location travels with them.
var (
	ErrNegativeTimestamp = errors.New("negative timestamp")
	ErrZeroDuration      = errors.New("zero or negative duration")
	ErrWrap              = errors.New("non-monotonic timestamp (wrap)")
	ErrInvalidTimeBase   = errors.New("invalid rational time base")
	ErrOverflow          = errors.New("timestamp rescale overflow")
)

// Rescale converts ticks expressed in time base src into time base dst using
// exact big-integer arithmetic, rounding half away from zero. It never uses
// floating point, so repeated conversions are deterministic.
func Rescale(ticks int64, src, dst Rational) (int64, error) {
	if !src.Valid() || !dst.Valid() {
		return 0, ErrInvalidTimeBase
	}
	// A Rational Num/Den means "Num ticks per Den seconds". A duration of
	// `ticks` in src therefore equals ticks*src.Den/src.Num seconds, and in dst
	// units is multiplied by dst.Num/dst.Den:
	//   out = ticks * src.Den * dst.Num / (src.Num * dst.Den)
	num := new(big.Int).SetInt64(ticks)
	num.Mul(num, big.NewInt(src.Den))
	num.Mul(num, big.NewInt(dst.Num))
	den := new(big.Int).Mul(big.NewInt(src.Num), big.NewInt(dst.Den))

	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(num, den, rem) // truncation toward zero

	// Round half away from zero: compare 2*|rem| with den.
	twice := new(big.Int).Abs(rem)
	twice.Mul(twice, big.NewInt(2))
	if twice.Cmp(den) >= 0 {
		if num.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	if !q.IsInt64() {
		return 0, ErrOverflow
	}
	return q.Int64(), nil
}

// CheckNonNegative rejects negative presentation/decoder timestamps.
func CheckNonNegative(ts int64) error {
	if ts < 0 {
		return ErrNegativeTimestamp
	}
	return nil
}

// CheckDuration rejects zero-length and negative edit/sample durations.
func CheckDuration(d int64) error {
	if d <= 0 {
		return ErrZeroDuration
	}
	return nil
}

// CheckMonotonic rejects a timestamp that moves backwards relative to the
// previous one on the same timeline. A legal 32-bit RTP rollover is unwrapped
// at the RTP layer (see UnwrapRTP); anything reaching the timeline as a
// backwards jump is a located wrap fault.
func CheckMonotonic(prev, cur int64) error {
	if cur < prev {
		return ErrWrap
	}
	return nil
}

// UnwrapRTP converts a pair of consecutive 32-bit RTP timestamps into a
// signed delta on the unwrapped 64-bit timeline. A modular rollover is a
// normal event (wrapped=true) and yields a positive delta; only a backwards
// jump larger than half the cycle is treated as suspicious.
func UnwrapRTP(prev, cur uint32) (delta int64, wrapped bool, err error) {
	const half = int64(1) << 31
	d := int64(int32(cur - prev)) // signed modular difference
	if cur < prev {
		wrapped = true
	}
	if d <= 0 {
		// Same tick (zero duration) or a backwards jump beyond half a cycle.
		if d == 0 {
			return 0, wrapped, ErrZeroDuration
		}
		return d, wrapped, ErrWrap
	}
	if d > half {
		return d, wrapped, ErrWrap
	}
	return d, wrapped, nil
}
