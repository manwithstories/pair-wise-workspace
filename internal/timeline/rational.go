// Package timeline defines the integer rational time base and the shared
// clock / error model used by every mediaflow subsystem. All time math is
// exact integer arithmetic; float is only used at reporting boundaries.
package timeline

import (
	"fmt"
	"math"
)

// Standard time scale: microseconds since the timeline origin. Every track is
// normalized to this scale so cross-track events can be merged deterministically.
const StandardScale int64 = 1_000_000

// Common media time bases.
var (
	RationalZero = Rational{}
	RationalOne  = Rational{num: 1, den: 1}
	// RTP / MPEG-TS PCR-style 90 kHz clock.
	Rational90k = Rational{num: 1, den: 90_000}
	// AAC audio frame clock (1024 samples per packet at 48 kHz).
	Rational48k = Rational{num: 1, den: 48_000}
	Microsecond = Rational{num: 1, den: StandardScale}
)

// Rational is a normalized, non-negative-denominator fraction num/den.
// The zero value is invalid for time bases (den == 0); construct values with
// NewRational, which normalizes and rejects zero denominators.
type Rational struct {
	num int64
	den int64
}

// NewRational builds a normalized rational. den must be non-zero.
func NewRational(num, den int64) (Rational, error) {
	if den == 0 {
		return RationalZero, fmt.Errorf("%w: rational denominator is zero", ErrInvalidTimebase)
	}
	if den < 0 {
		num, den = -num, -den
	}
	if num == 0 {
		return Rational{num: 0, den: den}, nil
	}
	g := gcd(abs64(num), den)
	return Rational{num: num / g, den: den / g}, nil
}

// MustRational panics on invalid input; intended for package constants only.
func MustRational(num, den int64) Rational {
	r, err := NewRational(num, den)
	if err != nil {
		panic(err)
	}
	return r
}

func (r Rational) Num() int64 { return r.num }
func (r Rational) Den() int64 { return r.den }

// Valid reports whether the rational carries a usable denominator.
func (r Rational) Valid() bool { return r.den > 0 }

func (r Rational) String() string { return fmt.Sprintf("%d/%d", r.num, r.den) }

// Equal compares two rationals in normalized form.
func (r Rational) Equal(o Rational) bool { return r.num == o.num && r.den == o.den }

// Mul returns r*o as an exact normalized fraction.
func (r Rational) Mul(o Rational) (Rational, error) {
	if !r.Valid() || !o.Valid() {
		return RationalZero, fmt.Errorf("%w: multiply on invalid rational", ErrInvalidTimebase)
	}
	if mulOverflows(r.num, o.num) || mulOverflows(r.den, o.den) {
		return RationalZero, fmt.Errorf("%w: rational multiply overflow %s*%s", ErrOverflow, r, o)
	}
	return NewRational(r.num*o.num, r.den*o.den)
}

// Inv returns den/num; the numerator must be non-zero.
func (r Rational) Inv() (Rational, error) {
	if !r.Valid() || r.num == 0 {
		return RationalZero, fmt.Errorf("%w: inverse of %s", ErrInvalidTimebase, r)
	}
	return NewRational(r.den, r.num)
}

// RescaleInt converts value expressed in src units into dst units with
// rounding to nearest, ties away from zero. It returns an error when the
// intermediate multiplication overflows int64 instead of silently wrapping.
func RescaleInt(value int64, src, dst Rational) (int64, error) {
	if !src.Valid() || !dst.Valid() {
		return 0, fmt.Errorf("%w: rescale with invalid timebase src=%s dst=%s", ErrInvalidTimebase, src, dst)
	}
	// value * src.num * dst.den / (src.den * dst.num)
	if mulOverflows(value, src.num) || mulOverflows(src.den, dst.num) {
		return 0, fmt.Errorf("%w: rescale overflow value=%d src=%s dst=%s", ErrOverflow, value, src, dst)
	}
	num := value * src.num * dst.den
	den := src.den * dst.num
	return roundedDiv(num, den), nil
}

// ToStandard converts a track tick count into microseconds on the shared scale.
func ToStandard(ticks int64, tb Rational) (int64, error) {
	return RescaleInt(ticks, tb, Microsecond)
}

// FromStandard converts microseconds back into a track's tick count.
func FromStandard(micros int64, tb Rational) (int64, error) {
	return RescaleInt(micros, Microsecond, tb)
}

// Cmp compares two rational values a/b and c/d exactly via cross products.
// Returns -1, 0 or 1.
func Cmp(aNum, aDen, cNum, cDen int64) (int, error) {
	if aDen <= 0 || cDen <= 0 {
		return 0, fmt.Errorf("%w: compare with non-positive denominator", ErrInvalidTimebase)
	}
	if mulOverflows(aNum, cDen) || mulOverflows(cNum, aDen) {
		return 0, fmt.Errorf("%w: rational compare overflow", ErrOverflow)
	}
	l, r := aNum*cDen, cNum*aDen
	switch {
	case l < r:
		return -1, nil
	case l > r:
		return 1, nil
	default:
		return 0, nil
	}
}

// Duration is an integer microsecond span on the normalized timeline.
type Duration int64

// Millis converts the duration to milliseconds (float only at the boundary).
func (d Duration) Millis() float64 { return float64(d) / 1000.0 }

// Micros returns the raw integer span.
func (d Duration) Micros() int64 { return int64(d) }

// FrameInterval is a half-open [Start, End) interval in standard microseconds.
type FrameInterval struct {
	Start int64
	End   int64
}

// NewInterval rejects negative durations, which indicate corrupt timestamps.
func NewInterval(start, end int64) (FrameInterval, error) {
	if start < 0 || end < start {
		return FrameInterval{}, fmt.Errorf("%w: interval [%d,%d)", ErrNegativeDuration, start, end)
	}
	return FrameInterval{Start: start, End: end}, nil
}

func (i FrameInterval) Duration() Duration { return Duration(i.End - i.Start) }
func (i FrameInterval) Empty() bool        { return i.End <= i.Start }

// Contains reports whether t falls inside the half-open interval.
func (i FrameInterval) Contains(t int64) bool { return t >= i.Start && t < i.End }

// Overlaps reports whether two half-open intervals share any instant.
func (i FrameInterval) Overlaps(o FrameInterval) bool {
	return i.Start < o.End && o.Start < i.End
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func abs64(v int64) int64 {
	if v < 0 {
		if v == math.MinInt64 {
			return math.MaxInt64
		}
		return -v
	}
	return v
}

func mulOverflows(a, b int64) bool {
	if a == 0 || b == 0 {
		return false
	}
	return a > math.MaxInt64/b || a < math.MinInt64/b
}

// roundedDiv divides with round-half-away-from-zero semantics.
func roundedDiv(num, den int64) int64 {
	q, r := num/den, num%den
	if (r > 0) == (den > 0) {
		if abs64(r)*2 >= abs64(den) {
			return q + sign64(den)
		}
	} else if abs64(r)*2 >= abs64(den) {
		return q - sign64(den)
	}
	return q
}

func sign64(v int64) int64 {
	if v > 0 {
		return 1
	}
	if v < 0 {
		return -1
	}
	return 0
}
