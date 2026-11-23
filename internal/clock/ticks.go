// Package clock defines the unified integer time base and exact rational
// conversions between MP4 track timescales, RTP clock rates and the internal
// microsecond tick grid. All arithmetic is integer-only and overflow checked;
// floating point must never be used to accumulate media timing.
package clock

import (
	"errors"
	"math"
	"math/bits"
)

// TicksPerSecond is the unified time base: one million ticks per second
// (each tick is one microsecond). Every track and RTP stream is mapped onto
// this grid so results from different subsystems share one timeline.
const TicksPerSecond int64 = 1_000_000

// Standard RTP clock rates (RFC 3551).
const (
	RTPVideo90k int64 = 90_000 // video / RFC 3551 90 kHz clock
	RTPCDR8k   int64 = 8_000  // PCM/telephone audio 8 kHz
)

// MaxTimescale bounds accepted MP4 timescales and RTP clock rates. A value
// above this is rejected rather than risking overflow in conversions.
const MaxTimescale int64 = 100_000_000

var (
	// ErrInvalidTimescale is returned for zero, negative or absurd clock rates.
	ErrInvalidTimescale = errors.New("clock: invalid timescale (must be 1..1e8)")
	// ErrNegativeDuration is returned when a duration below zero is encountered.
	ErrNegativeDuration = errors.New("clock: negative duration")
	// ErrOverflow is returned when an integer conversion cannot fit in int64.
	ErrOverflow = errors.New("clock: integer overflow in time conversion")
	// ErrInvalidRational is returned for a zero or negative denominator.
	ErrInvalidRational = errors.New("clock: rational denominator must be positive")
)

// Rational is an exact numerator/denominator pair. Denominator is always > 0.
type Rational struct {
	Num int64
	Den int64
}

// NewRational validates and constructs a Rational.
func NewRational(num, den int64) (Rational, error) {
	if den <= 0 {
		return Rational{}, ErrInvalidRational
	}
	if num == 0 {
		return Rational{Num: 0, Den: den}, nil
	}
	// Normalize by gcd to keep intermediate products small.
	g := gcd(abs64(num), den)
	return Rational{Num: num / g, Den: den / g}, nil
}

// Rate returns the Rational converting one unit of a clock running at
// timescale units/second into unified ticks.
func Rate(timescale int64) (Rational, error) {
	if timescale <= 0 || timescale > MaxTimescale {
		return Rational{}, ErrInvalidTimescale
	}
	return NewRational(TicksPerSecond, timescale)
}

// Scale computes value*r.Num/r.Den truncated toward zero, with full int64
// overflow checks on every intermediate step.
func (r Rational) Scale(value int64) (int64, error) {
	if r.Num == 0 || value == 0 {
		return 0, nil
	}
	if r.Den <= 0 {
		return 0, ErrInvalidRational
	}
	// value*Num/Den = (value/Den)*Num + (value%Den)*Num/Den, which keeps
	// every intermediate product bounded and avoids 128-bit division.
	whole := value / r.Den
	frac := value % r.Den
	a, ok := mul64(whole, r.Num)
	if !ok {
		return 0, ErrOverflow
	}
	bProd, ok := mul64(frac, r.Num)
	if !ok {
		return 0, ErrOverflow
	}
	b := bProd / r.Den // |bProd| intermediate still fits int64 (checked above)
	res, ok := add64(a, b)
	if !ok {
		return 0, ErrOverflow
	}
	return res, nil
}

// ScaleRound is Scale with rounding to the nearest tick (half away from zero).
func (r Rational) ScaleRound(value int64) (int64, error) {
	if value == 0 {
		return 0, nil
	}
	if r.Num == 0 {
		return 0, ErrInvalidRational
	}
	if r.Den <= 0 {
		return 0, ErrInvalidRational
	}
	num, ok := mul64(value, r.Num)
	if !ok {
		return 0, ErrOverflow
	}
	// Round half away from zero using absolute-value arithmetic, which is
	// immune to Go's truncation-toward-zero of signed division.
	neg := num < 0
	absNum := num
	if neg {
		absNum = -num
	}
	half := r.Den / 2
	q, ok := add64(absNum, half)
	if !ok {
		return 0, ErrOverflow
	}
	q /= r.Den
	if neg {
		return -q, nil
	}
	return q, nil
}

// FromClock maps a duration expressed in units of the given clock rate into
// unified ticks. Negative durations are rejected with ErrNegativeDuration.
func FromClock(units, timescale int64) (int64, error) {
	if units < 0 {
		return 0, ErrNegativeDuration
	}
	rate, err := Rate(timescale)
	if err != nil {
		return 0, err
	}
	return rate.Scale(units)
}

// ValidateDuration rejects negative tick durations.
func ValidateDuration(ticks int64) error {
	if ticks < 0 {
		return ErrNegativeDuration
	}
	return nil
}

// mul64 returns the signed product and ok=false on int64 overflow.
func mul64(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	// math.MinInt64 has no representable absolute value; its product only
	// fits when the other operand is 0 or +/-1.
	if a == math.MinInt64 || b == math.MinInt64 {
		other := b
		if b == math.MinInt64 {
			other = a
		}
		switch other {
		case 0:
			return 0, true
		case 1:
			return math.MinInt64, true
		default:
			// other == -1 would yield 2^63, which does not fit in int64.
			return 0, false
		}
	}
	x := a
	if x < 0 {
		x = -x
	}
	y := b
	if y < 0 {
		y = -y
	}
	hi, lo := bits.Mul64(uint64(x), uint64(y))
	// The unsigned absolute product fits in signed int64 exactly when hi==0
	// and the top bit of lo is clear (|product| <= math.MaxInt64).
	if hi != 0 || lo > uint64(math.MaxInt64) {
		return 0, false
	}
	res := int64(lo)
	if (a < 0) != (b < 0) {
		return -res, true
	}
	return res, true
}

// add64 returns the signed sum and ok=false on int64 overflow.
func add64(a, b int64) (int64, bool) {
	lo, carry := bits.Add64(uint64(a), uint64(b), 0)
	sum := int64(lo)
	if (a >= 0 && b >= 0 && sum < 0) || (a < 0 && b < 0 && sum >= 0) {
		_ = carry
		return 0, false
	}
	return sum, true
}

// sub64 returns the signed difference and ok=false on int64 overflow.
func sub64(a, b int64) (int64, bool) {
	lo, borrow := bits.Sub64(uint64(a), uint64(b), 0)
	diff := int64(lo)
	if (a >= 0 && b < 0 && diff < 0) || (a < 0 && b >= 0 && diff >= 0) {
		_ = borrow
		return 0, false
	}
	return diff, true
}

func abs64(v int64) int64 {
	if v >= 0 {
		return v
	}
	return -v
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
