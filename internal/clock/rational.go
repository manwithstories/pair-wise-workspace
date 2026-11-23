// Rational numbers and the single rounding policy shared by every
// conversion. Kept separate from Timeline so the error-model and interval
// code can be reviewed without scrolling past the big-number helpers.
package clock

import (
	"errors"
	"fmt"
	"math"
	"math/big"
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

// Timebase is a rational number of integer ticks per second.
type Timebase struct {
	Rate Rational
}

// NewTimebase requires a strictly positive rate.
func NewTimebase(rate Rational) (Timebase, error) {
	if !rate.validRate() {
		return Timebase{}, fmt.Errorf("%w: %s", ErrInvalidRate, rate)
	}
	return Timebase{Rate: rate}, nil
}

// MustTimebase is for constants.
func MustTimebase(num, den int64) Timebase {
	tb, err := NewTimebase(MustRational(num, den))
	if err != nil {
		panic(err)
	}
	return tb
}

// Rescale converts an integer tick count from one timebase to another.
// The result is exact whenever the rational division is exact; otherwise
// the supplied rounding policy is applied. Overflow is always reported.
func Rescale(ticks int64, from, to Rational, mode Rounding) (int64, error) {
	if !mode.valid() {
		return 0, fmt.Errorf("%w: %d", ErrInvalidRounding, int(mode))
	}
	if !from.validRate() || !to.validRate() {
		return 0, fmt.Errorf("%w: from=%s to=%s", ErrInvalidRate, from, to)
	}
	// ticks/from * to = ticks * to.Num * from.Den / (from.Num * to.Den)
	num := new(big.Int).Mul(big.NewInt(ticks), big.NewInt(to.Num))
	num.Mul(num, big.NewInt(from.Den))
	den := new(big.Int).Mul(big.NewInt(from.Num), big.NewInt(to.Den))
	return bigToInt64(divRounded(num, den, mode))
}

func divRounded(num, den *big.Int, mode Rounding) *big.Int {
	q, r := new(big.Int).QuoRem(num, den, new(big.Int)) // truncates toward zero
	if r.Sign() == 0 {
		return q
	}
	switch mode {
	case RoundTowardZero:
		return q
	case RoundDown:
		if num.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		}
	case RoundUp:
		if num.Sign() > 0 {
			q.Add(q, big.NewInt(1))
		}
	case RoundNearest:
		twice := new(big.Int).Abs(r)
		twice.Lsh(twice, 1)
		if twice.Cmp(den) >= 0 {
			if num.Sign() < 0 {
				q.Sub(q, big.NewInt(1))
			} else {
				q.Add(q, big.NewInt(1))
			}
		}
	}
	return q
}

func bigToInt64(v *big.Int) (int64, error) {
	if !v.IsInt64() {
		return 0, fmt.Errorf("%w: %s", ErrOverflow, v.String())
	}
	return v.Int64(), nil
}

func reduceBig(n, d *big.Int) (*big.Int, *big.Int) {
	g := new(big.Int).GCD(nil, nil, new(big.Int).Abs(n), d)
	if g.Sign() == 0 || g.Cmp(big.NewInt(1)) == 0 {
		return n, d
	}
	return new(big.Int).Quo(n, g), new(big.Int).Quo(d, g)
}

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

// Positive reports a strictly positive fraction, i.e. a usable rate.
func (r Rational) Positive() bool { return r.Num > 0 && r.Den > 0 }
