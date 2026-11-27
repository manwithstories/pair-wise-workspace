// Package clock defines the rational timebase shared by all modules.
// Media time is an exact rational number of seconds (Num/Den), so
// tracks with different timescales can be unified without rounding.
package clock

import (
	"errors"
	"fmt"
	"math"
	"math/big"
)

var (
	// ErrOverflow reports integer overflow in time arithmetic.
	ErrOverflow = errors.New("clock: integer overflow")
	// ErrNegativeDuration reports a negative duration.
	ErrNegativeDuration = errors.New("clock: negative duration")
	// ErrWraparound reports an illegal timestamp wraparound.
	ErrWraparound = errors.New("clock: illegal timestamp wraparound")
)

// Time is a rational media time in seconds. Den is always positive
// and the fraction is kept reduced.
type Time struct {
	Num int64
	Den int64
}

func gcd(a, b int64) int64 {
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

// New builds a normalized Time, rejecting a zero denominator.
func New(num, den int64) (Time, error) {
	if den == 0 {
		return Time{}, fmt.Errorf("clock: zero denominator")
	}
	if den < 0 {
		if num == math.MinInt64 || den == math.MinInt64 {
			return Time{}, ErrOverflow
		}
		num, den = -num, -den
	}
	if g := gcd(num, den); g > 1 {
		num /= g
		den /= g
	}
	return Time{Num: num, Den: den}, nil
}

// Must is New for constants; it panics on error.
func Must(num, den int64) Time {
	t, err := New(num, den)
	if err != nil {
		panic(err)
	}
	return t
}

// IsNegative reports whether t < 0.
func (t Time) IsNegative() bool { return t.Num < 0 }

// String renders the exact rational form, e.g. "3000/1001".
func (t Time) String() string { return fmt.Sprintf("%d/%d", t.Num, t.Den) }

// Seconds returns t as a float64, for rendering only.
func (t Time) Seconds() float64 { return float64(t.Num) / float64(t.Den) }

func checkedMul(a, b int64) (int64, error) {
	r := a * b
	if a != 0 && r/a != b {
		return 0, ErrOverflow
	}
	return r, nil
}

func checkedAdd(a, b int64) (int64, error) {
	r := a + b
	if (b > 0 && r < a) || (b < 0 && r > a) {
		return 0, ErrOverflow
	}
	return r, nil
}

// Add returns t+o with overflow checking.
func (t Time) Add(o Time) (Time, error) {
	n1, err := checkedMul(t.Num, o.Den)
	if err != nil {
		return Time{}, err
	}
	n2, err := checkedMul(o.Num, t.Den)
	if err != nil {
		return Time{}, err
	}
	n, err := checkedAdd(n1, n2)
	if err != nil {
		return Time{}, err
	}
	d, err := checkedMul(t.Den, o.Den)
	if err != nil {
		return Time{}, err
	}
	return New(n, d)
}

// Sub returns t-o with overflow checking.
func (t Time) Sub(o Time) (Time, error) {
	if o.Num == math.MinInt64 {
		return Time{}, ErrOverflow
	}
	return t.Add(Time{Num: -o.Num, Den: o.Den})
}

// Compare returns -1, 0 or 1. It falls back to big integers when the
// cross product would overflow int64, so it is always exact.
func (t Time) Compare(o Time) int {
	l, err1 := checkedMul(t.Num, o.Den)
	r, err2 := checkedMul(o.Num, t.Den)
	if err1 != nil || err2 != nil {
		bl := big.NewInt(t.Num)
		bl.Mul(bl, big.NewInt(o.Den))
		br := big.NewInt(o.Num)
		br.Mul(br, big.NewInt(t.Den))
		return bl.Cmp(br)
	}
	switch {
	case l < r:
		return -1
	case l > r:
		return 1
	default:
		return 0
	}
}

// CheckMonotonic rejects cur < prev as an illegal wraparound.
func CheckMonotonic(prev, cur Time) error {
	if cur.Compare(prev) < 0 {
		return fmt.Errorf("%w: %s < %s", ErrWraparound, cur, prev)
	}
	return nil
}

// RequireNonNegative rejects negative durations.
func RequireNonNegative(d Time) error {
	if d.IsNegative() {
		return fmt.Errorf("%w: %s", ErrNegativeDuration, d)
	}
	return nil
}
