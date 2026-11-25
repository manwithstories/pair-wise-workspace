// Package media holds the shared time model and error model used by every
// subsystem of avcheck. Timeline arithmetic is integer rubrics: timestamps are
// converted with a signed 128-bit rational value and never accumulated in
// floating point.
package media

import (
	"fmt"
	"math"
	"math/big"
	"time"
)

// TrackKind identifies the role of a track in the dependency graph.
type TrackKind uint8

const (
	KindUnknown TrackKind = iota
	KindVideo
	KindAudio
	KindCue
)

func (k TrackKind) String() string {
	switch k {
	case KindVideo:
		return "video"
	case KindAudio:
		return "audio"
	case KindCue:
		return "cue"
	default:
		return "unknown"
	}
}

// ParseTrackKind maps a textual track type onto a TrackKind.
func ParseTrackKind(s string) (TrackKind, bool) {
	switch s {
	case "video", "V":
		return KindVideo, true
	case "audio", "A":
		return KindAudio, true
	case "cue", "cues", "subtitle", "text", "T":
		return KindCue, true
	}
	return KindUnknown, false
}

// Rational is an exact non-negative time scale numerator/denominator.
// It is used for source timeline ticks, never for accumulated drift.
type Rational struct {
	Num int64
	Den int64
}

// Valid reports whether the rational describes a usable positive rate.
func (r Rational) Valid() bool { return r.Num > 0 && r.Den > 0 }

// Nanos converts n ticks into nanoseconds using exact integer rounding
// (half away from zero). It saturates instead of overflowing.
func Nanos(n int64, r Rational) int64 {
	if !r.Valid() {
		return 0
	}
	x := new(big.Int).Mul(big.NewInt(n), big.NewInt(r.Num))
	d := big.NewInt(r.Den)
	q, rem := new(big.Int).QuoRem(x, d, new(big.Int))
	// Round half away from zero: 2*|rem| >= d increments the magnitude.
	twiceRem := new(big.Int).Abs(rem)
	twiceRem.Lsh(twiceRem, 1)
	if twiceRem.Cmp(d) >= 0 {
		if x.Sign() >= 0 {
			q.Add(q, big.NewInt(1))
		} else {
			q.Sub(q, big.NewInt(1))
		}
	}
	return clampInt64(q)
}

// NanosPerTick is Nanos(1, r): the size of one tick in nanoseconds.
func NanosPerTick(r Rational) int64 { return Nanos(1, r) }

// NanosSince returns the exact elapsed nanoseconds between two ticks.
func NanosSince(from, to int64, r Rational) int64 {
	if to <= from {
		return 0
	}
	return Nanos(to-from, r)
}

func clampInt64(x *big.Int) int64 {
	if x.IsInt64() {
		return x.Int64()
	}
	if x.Sign() > 0 {
		return math.MaxInt64
	}
	return math.MinInt64
}

// TicksToDuration converts ticks at rate r to a time.Duration, saturating.
func TicksToDuration(ticks int64, r Rational) time.Duration {
	return time.Duration(Nanos(ticks, r))
}

// DurationToTicks converts a duration to ticks at rate r (rounded half up).
func DurationToTicks(d time.Duration, r Rational) int64 {
	if !r.Valid() {
		return 0
	}
	x := new(big.Int).Mul(big.NewInt(int64(d)), big.NewInt(r.Den))
	num := big.NewInt(r.Num)
	q, rem := new(big.Int).QuoRem(x, num, new(big.Int))
	twiceRem := new(big.Int).Abs(rem)
	twiceRem.Lsh(twiceRem, 1)
	if twiceRem.Cmp(num) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	return clampInt64(q)
}

// Percent describes pct of num/den as an exact float64 percentage.
func Percent(num, den int64) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) * 100 / float64(den)
}

// FormatTicks renders a tick value with its scale for diagnostics.
func FormatTicks(ticks int64, r Rational) string {
	return fmt.Sprintf("%d/%d ticks (%d ns)", ticks, r.Den, Nanos(ticks, r))
}
