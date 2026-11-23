// Rational numbers and the single rounding policy shared by every
// conversion. Kept separate from Timeline so the error-model and interval
// code can be reviewed without scrolling past the big-number helpers.
package clock

import (
	"errors"
	"fmt"
	"math"
)

// Sentinels shared by the timebase and the classified error model.
var (
	ErrZeroDenominator  = errors.New("clock: zero denominator")
	ErrOverflow         = errors.New("clock: time conversion overflow")
	ErrNegativeDuration = errors.New("clock: negative duration")
	ErrInvalidRate      = errors.New("clock: rate must be positive")
	ErrInvalidRounding  = errors.New("clock: invalid rounding mode")
)

// Rational is an exact reduced fraction with a positive denominator.
type Rational struct {
	Num int64
	Den int64
}

// NewRational validates, normalizes and reduces a fraction.
func NewRational(num, den int64) (Rational, error) {
	if den == 0 {
		return Rational{}, ErrZeroDenominator
	}
	if den < 0 {
		if num == math.MinInt64 || den == math.MinInt64 {
			return Rational{}, fmt.Errorf("%w: num=%d den=%d", ErrOverflow, num, den)
		}
		num, den = -num, -den
	}
	if num == 0 {
		return Rational{Num: 0, Den: 1}, nil
	}
	g := gcd(absInt64(num), den)
	return Rational{Num: num / g, Den: den / g}, nil
}

// MustRational is for compile-time constants in fixtures and tests.
func MustRational(num, den int64) Rational {
	r, err := NewRational(num, den)
	if err != nil {
		panic(err)
	}
	return r
}

// Float64 returns the approximate floating point value (reporting only).
func (r Rational) Float64() float64 { return float64(r.Num) / float64(r.Den) }

// Sign reports -1, 0 or 1.
func (r Rational) Sign() int {
	switch {
	case r.Num < 0:
		return -1
	case r.Num > 0:
		return 1
	default:
		return 0
	}
}

// String renders the fraction as "num/den".
func (r Rational) String() string {
	if r.Den == 1 {
		return fmt.Sprintf("%d", r.Num)
	}
	return fmt.Sprintf("%d/%d", r.Num, r.Den)
}

func (r Rational) validRate() bool { return r.Num > 0 && r.Den > 0 }

// Rounding selects the single policy applied to every inexact conversion.
type Rounding int

const (
	// RoundNearest rounds half away from zero.
	RoundNearest Rounding = iota
	// RoundDown rounds towards negative infinity.
	RoundDown
	// RoundUp rounds towards positive infinity.
	RoundUp
	// RoundTowardZero truncates.
	RoundTowardZero
)

// DefaultRounding is used by callers that do not need a special policy.
const DefaultRounding = RoundNearest

func (m Rounding) String() string {
	switch m {
	case RoundNearest:
		return "nearest"
	case RoundDown:
		return "down"
	case RoundUp:
		return "up"
	case RoundTowardZero:
		return "toward_zero"
	default:
		return "invalid"
	}
}

func (m Rounding) valid() bool { return m >= RoundNearest && m <= RoundTowardZero }

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
