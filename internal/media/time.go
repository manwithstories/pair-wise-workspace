// Package media defines the shared primitives used by every mediad subsystem:
// rational media time bases, a common event timestamp and classified errors.
package media

import (
	"fmt"
	"math/big"
	"strings"
)

// MaxTimebaseNum bounds the numerator and denominator accepted for a media
// time base. Values beyond it (or arithmetic that exceeds it) are reported as
// rational overflow instead of silently wrapping.
const MaxTimebaseNum = 1 << 31

// TickRate is the denominator used by MediaTick: every event timestamp is
// carried in units of 1/TickRate seconds so clock, timeline and audio code
// share one exact integer domain.
const TickRate = 48000

// Rational is an exact media time base: one raw tick equals Num/Den seconds.
type Rational struct {
	Num int64
	Den int64
}

// NewRational validates and constructs a time base. The denominator must be
// positive, both parts must be non-zero and fit in MaxTimebaseNum.
func NewRational(num, den int64) (Rational, error) {
	if num <= 0 || den <= 0 {
		return Rational{}, NewError(ErrInvalidTimebase, "timebase must be positive: %d/%d", num, den)
	}
	if num > MaxTimebaseNum || den > MaxTimebaseNum {
		return Rational{}, NewError(ErrInvalidTimebase, "timebase overflows: %d/%d", num, den)
	}
	r := Rational{Num: num, Den: den}
	r.reduce()
	return r, nil
}

func (r Rational) reduce() {
	g := gcd64(r.Num, r.Den)
	if g > 1 {
		r.Num /= g
		r.Den /= g
	}
}

// String renders the time base as "num/den".
func (r Rational) String() string { return fmt.Sprintf("%d/%d", r.Num, r.Den) }

// Float64 returns the seconds value of one tick.
func (r Rational) Float64() float64 { return float64(r.Num) / float64(r.Den) }

// TicksToTicks rescales an integer tick count from this time base into dst,
// rounding to the nearest tick. Both results and intermediates are range
// checked; an overflow returns an ErrInvalidTimebase MediaError.
func (r Rational) TicksToTicks(ticks int64, dst Rational) (int64, error) {
	if r.Den == 0 || dst.Den == 0 {
		return 0, NewError(ErrInvalidTimebase, "zero denominator in rescale %s -> %s", r, dst)
	}
	// ticks * r.Num/dst.Num * dst.Den/r.Den, done exactly with big.Int.
	num := new(big.Int).Mul(big.NewInt(ticks), big.NewInt(r.Num))
	num.Mul(num, big.NewInt(dst.Den))
	den := new(big.Int).Mul(big.NewInt(r.Den), big.NewInt(dst.Num))
	if den.Sign() == 0 {
		return 0, NewError(ErrInvalidTimebase, "zero denominator in rescale %s -> %s", r, dst)
	}
	// Round half away from zero.
	half := new(big.Int).Abs(new(big.Int).Set(den))
	half.Rsh(half, 1)
	neg := num.Sign() < 0
	if neg {
		num.Neg(num)
	}
	num.Add(num, half)
	num.Quo(num, new(big.Int).Abs(den))
	if neg {
		num.Neg(num)
	}
	if !num.IsInt64() {
		return 0, NewError(ErrInvalidTimebase, "rescale %d ticks from %s to %s overflows int64", ticks, r, dst)
	}
	return num.Int64(), nil
}

// ToTicks converts raw ticks in this time base to MediaTick units.
func (r Rational) ToTicks(ticks int64) (MediaTick, error) {
	v, err := r.TicksToTicks(ticks, Rational{Num: 1, Den: TickRate})
	if err != nil {
		return 0, err
	}
	return MediaTick(v), nil
}

// ParseRational parses "num/den" (also accepting "num:den") into a time base.
func ParseRational(s string) (Rational, error) {
	s = strings.TrimSpace(s)
	sep := "/"
	if strings.Contains(s, ":") && !strings.Contains(s, "/") {
		sep = ":"
	}
	parts := strings.Split(s, sep)
	if len(parts) != 2 {
		return Rational{}, NewError(ErrInvalidTimebase, "malformed timebase %q", s)
	}
	var num, den int64
	if _, err := fmt.Sscanf(parts[0], "%d", &num); err != nil {
		return Rational{}, NewError(ErrInvalidTimebase, "malformed timebase numerator %q", parts[0])
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &den); err != nil {
		return Rational{}, NewError(ErrInvalidTimebase, "malformed timebase denominator %q", parts[1])
	}
	return NewRational(num, den)
}

// MediaTick is a timestamp in units of 1/TickRate seconds. Every subsystem
// carries event time as MediaTick so no float rounding leaks between stages.
type MediaTick int64

// Seconds returns the tick value as seconds.
func (t MediaTick) Seconds() float64 { return float64(t) / float64(TickRate) }

// TicksAtSample maps a sample index at the given sample rate to a MediaTick.
func TicksAtSample(sample int64, rate int) (MediaTick, error) {
	if rate <= 0 {
		return 0, NewError(ErrInvalidTimebase, "invalid sample rate %d", rate)
	}
	return Rational{Num: 1, Den: int64(rate)}.ToTicks(sample)
}

func gcd64(a, b int64) int64 {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
