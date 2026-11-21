// Package model defines the shared media time model and the cross-subsystem
// fault model used by manifests, scanning, stream analysis, recovery and
// provenance reporting.
package model

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Time model errors. They are deliberately shared so every subsystem reports
// the same classification for a rejected timeline.
var (
	ErrInvalidTimebase = errors.New("model: invalid timescale")
	ErrNegativeTime    = errors.New("model: negative duration")
	ErrTimeOverflow    = errors.New("model: time arithmetic overflow")
	ErrInvalidRational = errors.New("model: invalid rational")
)

// MaxTimescale caps the rational clock denominators accepted from manifests.
// A 1e9 tick/s clock is far above anything found in HLS or DASH and keeps
// every intermediate product inside int64 for the fixtures used here.
const MaxTimescale = 1_000_000_000

// Rational is an exact media clock value in seconds, stored as a normalized
// fraction Num/Den with Den > 0. All timeline conversion flows through it so
// HLS decimal seconds and DASH integer ticks can be compared exactly.
type Rational struct {
	Num int64
	Den int64
}

// Zero is the additive identity of the media clock.
var Zero = Rational{Num: 0, Den: 1}

// NewRational validates, normalizes and reduces num/den.
func NewRational(num, den int64) (Rational, error) {
	if den == 0 {
		return Rational{}, fmt.Errorf("%w: zero denominator", ErrInvalidRational)
	}
	if num == math.MinInt64 || den == math.MinInt64 {
		return Rational{}, fmt.Errorf("%w: %d/%d", ErrTimeOverflow, num, den)
	}
	if den < 0 {
		num, den = -num, -den
	}
	g := gcd(abs64(num), den)
	return Rational{Num: num / g, Den: den / g}, nil
}

// MustRational is intended for compile-time constants and tests.
func MustRational(num, den int64) Rational {
	r, err := NewRational(num, den)
	if err != nil {
		panic(err)
	}
	return r
}

// FromTicks converts a media-tick count into seconds for the given timescale.
// A negative tick count is rejected: manifests may not declare negative
// durations, while signed offsets are modelled with Sub instead.
func FromTicks(ticks int64, timescale uint32) (Rational, error) {
	if err := CheckTimescale(timescale); err != nil {
		return Rational{}, err
	}
	if ticks < 0 {
		return Rational{}, fmt.Errorf("%w: %d ticks", ErrNegativeTime, ticks)
	}
	return NewRational(ticks, int64(timescale))
}

// CheckTimescale rejects zero and oversized rational clocks.
func CheckTimescale(ts uint32) error {
	if ts == 0 || ts > MaxTimescale {
		return fmt.Errorf("%w: %d", ErrInvalidTimebase, ts)
	}
	return nil
}

// FromString parses either "num/den" or a plain decimal number of seconds
// with at most nanosecond precision. It is used for HLS EXTINF values.
func FromString(s string) (Rational, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Rational{}, fmt.Errorf("%w: empty", ErrInvalidRational)
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		num, err := strconv.ParseInt(s[:i], 10, 64)
		if err != nil {
			return Rational{}, fmt.Errorf("%w: %q", ErrInvalidRational, s)
		}
		den, err := strconv.ParseInt(s[i+1:], 10, 64)
		if err != nil {
			return Rational{}, fmt.Errorf("%w: %q", ErrInvalidRational, s)
		}
		return NewRational(num, den)
	}
	neg := strings.HasPrefix(s, "-")
	body := strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")
	intPart, fracPart, _ := strings.Cut(body, ".")
	if intPart == "" {
		intPart = "0"
	}
	if len(fracPart) > 9 {
		fracPart = fracPart[:9]
	}
	for len(fracPart) < 9 {
		fracPart += "0"
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return Rational{}, fmt.Errorf("%w: %q", ErrInvalidRational, s)
	}
	frac, err := strconv.ParseInt(fracPart, 10, 64)
	if err != nil {
		return Rational{}, fmt.Errorf("%w: %q", ErrInvalidRational, s)
	}
	ticks, ok := add64(whole*1_000_000_000, frac)
	if !ok || whole > math.MaxInt64/1_000_000_000 {
		return Rational{}, fmt.Errorf("%w: %q", ErrTimeOverflow, s)
	}
	if neg {
		ticks = -ticks
	}
	return NewRational(ticks, 1_000_000_000)
}

func (r Rational) valid() bool { return r.Den > 0 }

// Add returns r+o with overflow detection.
func (r Rational) Add(o Rational) (Rational, error) {
	if !r.valid() || !o.valid() {
		return Rational{}, fmt.Errorf("%w: uninitialized fraction", ErrInvalidRational)
	}
	a, ok := mul64(r.Num, o.Den)
	if !ok {
		return Rational{}, fmt.Errorf("%w: add", ErrTimeOverflow)
	}
	b, ok := mul64(o.Num, r.Den)
	if !ok {
		return Rational{}, fmt.Errorf("%w: add", ErrTimeOverflow)
	}
	num, ok := add64(a, b)
	if !ok {
		return Rational{}, fmt.Errorf("%w: add", ErrTimeOverflow)
	}
	den, ok := mul64(r.Den, o.Den)
	if !ok {
		return Rational{}, fmt.Errorf("%w: add denominator", ErrTimeOverflow)
	}
	return NewRational(num, den)
}

// Sub returns r-o. Negative results are allowed; only durations are required
// to be non-negative by the model.
func (r Rational) Sub(o Rational) (Rational, error) {
	neg, err := NewRational(-o.Num, o.Den)
	if err != nil {
		return Rational{}, err
	}
	return r.Add(neg)
}

// MulInt multiplies the clock by an integer sample or segment count.
func (r Rational) MulInt(n int64) (Rational, error) {
	num, ok := mul64(r.Num, n)
	if !ok {
		return Rational{}, fmt.Errorf("%w: multiply", ErrTimeOverflow)
	}
	return NewRational(num, r.Den)
}

// Cmp returns -1, 0 or 1. It uses 128-bit intermediates via math/big only
// when the int64 cross-products would overflow, so the hot path stays exact
// and allocation-free.
func (r Rational) Cmp(o Rational) int {
	a, okA := mul64(r.Num, o.Den)
	b, okB := mul64(o.Num, r.Den)
	if !okA || !okB {
		bi := new(big.Int).Mul(big.NewInt(r.Num), big.NewInt(o.Den))
		bj := new(big.Int).Mul(big.NewInt(o.Num), big.NewInt(r.Den))
		return bi.Cmp(bj)
	}
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// Less reports r < o.
func (r Rational) Less(o Rational) bool { return r.Cmp(o) < 0 }

// Equal reports r == o after normalization.
func (r Rational) Equal(o Rational) bool { return r.Cmp(o) == 0 }

// Sign returns -1, 0 or 1 for the sign of the fraction.
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

// IsZero reports whether r is the zero clock value.
func (r Rational) IsZero() bool { return r.Num == 0 }

// MillisRounded returns the value rounded half away from zero, in ms, with
// overflow detection. This is the canonical form used in reports.
func (r Rational) MillisRounded() (int64, error) {
	if !r.valid() {
		return 0, fmt.Errorf("%w: uninitialized fraction", ErrInvalidRational)
	}
	neg := r.Num < 0
	num := r.Num
	if neg {
		if num == math.MinInt64 {
			return 0, fmt.Errorf("%w: milliseconds", ErrTimeOverflow)
		}
		num = -num
	}
	scaled, ok := mul64(num, 1000)
	if !ok {
		return 0, fmt.Errorf("%w: milliseconds", ErrTimeOverflow)
	}
	q, rem := scaled/r.Den, scaled%r.Den
	// Round half away from zero: compare 2*rem against the denominator.
	if rem > 0 && rem >= r.Den-rem {
		q++
	}
	if neg {
		q = -q
	}
	return q, nil
}

// String renders the normalized fraction.
func (r Rational) String() string { return fmt.Sprintf("%d/%d", r.Num, r.Den) }

func abs64(v int64) int64 {
	if v < 0 {
		if v == math.MinInt64 {
			return math.MaxInt64
		}
		return -v
	}
	return v
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		return 1
	}
	return a
}

func mul64(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a == math.MinInt64 || b == math.MinInt64 {
		return 0, false
	}
	r := a * b
	if r/b != a {
		return 0, false
	}
	return r, true
}

func add64(a, b int64) (int64, bool) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, false
	}
	return a + b, true
}
