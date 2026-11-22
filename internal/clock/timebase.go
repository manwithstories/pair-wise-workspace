// Package clock defines the shared rational media timebase and wraparound
// handling used by every subsystem. No wall-clock time lives here: all values
// are deterministic media ticks so that repeated runs produce identical output.
package clock

import (
	"fmt"
	"math/big"
)

// Rational is a media timebase expressed as Num/Den ticks per second.
// Den is always positive; the zero value is invalid and callers must
// construct timebases through New.
type Rational struct {
	Num int64
	Den int64
}

// New returns a normalized timebase. It panics on a non-positive denominator
// because every downstream conversion assumes a well-formed timebase.
func New(num, den int64) Rational {
	if den <= 0 || num <= 0 {
		panic(fmt.Sprintf("clock: invalid timebase %d/%d", num, den))
	}
	g := new(big.Int).GCD(nil, nil, big.NewInt(num), big.NewInt(den)).Int64()
	return Rational{Num: num / g, Den: den / g}
}

// Equals reports whether two timebases denote the same rate.
func (r Rational) Equals(o Rational) bool {
	return r.Num == o.Num && r.Den == o.Den
}

// Rescale converts ticks measured in src ticks/second into dst ticks/second
// with rounding to the nearest destination tick.
func Rescale(ticks int64, src, dst Rational) int64 {
	if src.Equals(dst) {
		return ticks
	}
	// ticks * dst.Num * src.Den / (src.Num * dst.Den), via big.Int so that
	// long-running 90kHz media counters cannot overflow int64 mid-expression.
	v := new(big.Int).SetInt64(ticks)
	v.Mul(v, big.NewInt(dst.Num))
	v.Mul(v, big.NewInt(src.Den))
	d := new(big.Int).Mul(big.NewInt(src.Num), big.NewInt(dst.Den))
	if v.Sign() >= 0 {
		v.Add(v, new(big.Int).Quo(d, big.NewInt(2)))
	} else {
		v.Sub(v, new(big.Int).Quo(d, big.NewInt(2)))
	}
	return v.Quo(v, d).Int64()
}

// Unwrap32 converts a stream of 32-bit wrapping counters (e.g. RTP 90kHz
// timestamps) into a monotonic int64 sequence. A discontinuity larger than
// half the modulus in either direction is interpreted as a wraparound.
func Unwrap32(prev int64, cur uint32) int64 {
	const mod = int64(1) << 32
	const half = mod / 2
	delta := int64(cur) - (prev & (mod - 1))
	if delta > half {
		delta -= mod
	} else if delta < -half {
		delta += mod
	}
	return prev + delta
}

// Unwrap16 is the 16-bit counterpart of Unwrap32, used for RTP sequence
// numbers and for the synthetic keyframe index counters.
func Unwrap16(prev int64, cur uint16) int64 {
	const mod = int64(1) << 16
	const half = mod / 2
	delta := int64(cur) - (prev & (mod - 1))
	if delta > half {
		delta -= mod
	} else if delta < -half {
		delta += mod
	}
	return prev + delta
}

// RTPClock is the fixed RTP video clock rate (RFC 6184/7798/6184).
func RTPClock() Rational { return Rational{Num: 90000, Den: 1} }

// Millis is the 1000 ticks/second timebase used for timeline summaries.
func Millis() Rational { return Rational{Num: 1000, Den: 1} }
