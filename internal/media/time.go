// Package media provides the shared exact time base and the unified error
// model used by every stage of the avpipe pipeline.
//
// All timestamps inside avpipe are carried as an exact rational number of
// seconds (Rational) or as an integer tick count anchored to a Rational time
// base (Time). Nothing in the pipeline converts a timestamp to float except
// for terminal display, so a timestamp can never drift because of rounding.
package media

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors shared by the time helpers.
var (
	ErrZeroDenominator = errors.New("media: zero denominator")
	ErrOverflow        = errors.New("media: rational overflow")
	ErrBadDuration     = errors.New("media: invalid duration")
)

// Rational is an exact fraction Num/Den expressed in seconds when used as a
// duration. Den is always positive and the fraction is always reduced.
type Rational struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

// NewRat builds a reduced rational number. A negative denominator is folded
// into the numerator. The zero value of Rational is invalid; callers can use
// Valid to check.
func NewRat(num, den int64) (Rational, error) {
	if den == 0 {
		return Rational{}, ErrZeroDenominator
	}
	if den < 0 {
		if num == math.MinInt64 {
			return Rational{}, ErrOverflow
		}
		num, den = -num, -den
	}
	g := gcd(abs64(num), den)
	if g > 1 {
		num, den = num/g, den/g
	}
	return Rational{Num: num, Den: den}, nil
}

// MustRat is NewRat for compile-time constant call sites.
func MustRat(num, den int64) Rational {
	r, err := NewRat(num, den)
	if err != nil {
		panic(err)
	}
	return r
}

// Valid reports whether the rational carries a usable denominator.
func (r Rational) Valid() bool { return r.Den > 0 }

// Neg returns -r.
func (r Rational) Neg() Rational { return Rational{Num: -r.Num, Den: r.Den} }

// Cmp compares a and b exactly and returns -1, 0 or 1.
func (r Rational) Cmp(o Rational) int {
	if !r.Valid() || !o.Valid() {
		panic("media: comparing invalid rational")
	}
	if r.Den == o.Den {
		return cmp64(r.Num, o.Num)
	}
	// Cross multiply in 128-bit space: both denominators are positive.
	var a, b big.Int
	a.SetInt64(r.Num)
	a.Mul(&a, big.NewInt(o.Den))
	b.SetInt64(o.Num)
	b.Mul(&b, big.NewInt(r.Den))
	return a.Cmp(&b)
}

// Equal reports exact equality.
func (r Rational) Equal(o Rational) bool { return r.Cmp(o) == 0 }

// Add returns r+o reduced.
func (r Rational) Add(o Rational) (Rational, error) {
	var n1, n2, d big.Int
	n1.SetInt64(r.Num)
	n1.Mul(&n1, big.NewInt(o.Den))
	n2.SetInt64(o.Num)
	n2.Mul(&n2, big.NewInt(r.Den))
	n1.Add(&n1, &n2)
	d.SetInt64(r.Den)
	d.Mul(&d, big.NewInt(o.Den))
	return ratFromBig(&n1, &d)
}

// Sub returns r-o reduced.
func (r Rational) Sub(o Rational) (Rational, error) {
	return r.Add(o.Neg())
}

// Mul returns r*o reduced.
func (r Rational) Mul(o Rational) (Rational, error) {
	var n, d big.Int
	n.SetInt64(r.Num)
	n.Mul(&n, big.NewInt(o.Num))
	d.SetInt64(r.Den)
	d.Mul(&d, big.NewInt(o.Den))
	return ratFromBig(&n, &d)
}

// MulInt returns r*n reduced.
func (r Rational) MulInt(n int64) (Rational, error) {
	return r.Mul(Rational{Num: n, Den: 1})
}

// Float64 renders the rational as a float. It must only be used for display.
func (r Rational) Float64() float64 {
	if !r.Valid() {
		return 0
	}
	return float64(r.Num) / float64(r.Den)
}

// String returns the canonical reduced "num/den" form. It is stable and is
// what the segment digests hash, so it never depends on formatting locale.
func (r Rational) String() string {
	if !r.Valid() {
		return "invalid"
	}
	return strconv.FormatInt(r.Num, 10) + "/" + strconv.FormatInt(r.Den, 10)
}

// SecondsString renders a human readable approximation for terminal output.
func (r Rational) SecondsString() string {
	return fmt.Sprintf("%.3fs", r.Float64())
}

// ParseDuration parses "2s", "1500ms", "1.5s" or an exact "num/den" number of
// seconds. Negative and dimensionless values are rejected.
func ParseDuration(s string) (Rational, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Rational{}, ErrBadDuration
	}
	if strings.Contains(s, "/") {
		parts := strings.Split(s, "/")
		if len(parts) != 2 {
			return Rational{}, ErrBadDuration
		}
		n, err1 := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		d, err2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err1 != nil || err2 != nil || d == 0 || n < 0 {
			return Rational{}, ErrBadDuration
		}
		return NewRat(n, d)
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return Rational{}, ErrBadDuration
	}
	return FromDuration(d), nil
}

// FromDuration converts a non-negative Go duration to exact seconds.
func FromDuration(d time.Duration) Rational {
	if d < 0 {
		d = -d
	}
	r, err := NewRat(int64(d), int64(time.Second))
	if err != nil {
		return Rational{Num: 0, Den: 1}
	}
	return r
}

// Duration converts a rational number of seconds to a Go duration, rounding
// to the nearest nanosecond.
func (r Rational) Duration() (time.Duration, error) {
	if !r.Valid() {
		return 0, ErrZeroDenominator
	}
	var n, d big.Int
	n.SetInt64(r.Num)
	n.Mul(&n, big.NewInt(int64(time.Second)))
	d.SetInt64(r.Den)
	q, rem := new(big.Int).QuoRem(&n, &d, new(big.Int))
	// Round half away from zero.
	twice := new(big.Int).Lsh(new(big.Int).Abs(rem), 1)
	if twice.Cmp(&d) >= 0 {
		if rem.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	if !q.IsInt64() {
		return 0, ErrOverflow
	}
	return time.Duration(q.Int64()), nil
}

// Time is an integer tick count anchored to an exact rational time base.
// Seconds(t) == Ticks * Base, computed exactly on demand.
type Time struct {
	Ticks int64  `json:"ticks"`
	Base  Rational `json:"base"`
}

// NewTime validates a timestamp.
func NewTime(ticks int64, base Rational) (Time, error) {
	if !base.Valid() || base.Num == 0 {
		return Time{}, fmt.Errorf("media: invalid time base %s", base)
	}
	return Time{Ticks: ticks, Base: base}, nil
}

// MustTime is NewTime for internal call sites that already validated the base.
func MustTime(ticks int64, base Rational) Time { return Time{Ticks: ticks, Base: base} }

// Seconds converts the timestamp to exact seconds.
func (t Time) Seconds() (Rational, error) {
	r, err := NewRat(t.Ticks, 1)
	if err != nil {
		return Rational{}, err
	}
	return r.Mul(t.Base)
}

// Cmp compares two timestamps exactly.
func (t Time) Cmp(o Time) int {
	a, err := t.Seconds()
	if err != nil {
		panic(err)
	}
	b, err := o.Seconds()
	if err != nil {
		panic(err)
	}
	return a.Cmp(b)
}

// Sub returns t-o in seconds.
func (t Time) Sub(o Time) (Rational, error) {
	a, err := t.Seconds()
	if err != nil {
		return Rational{}, err
	}
	b, err := o.Seconds()
	if err != nil {
		return Rational{}, err
	}
	return a.Sub(b)
}

// AddSeconds returns the timestamp advanced by d, rounded to the nearest tick
// of t.Base when d is not exactly representable.
func (t Time) AddSeconds(d Rational) (Time, error) {
	delta, err := rescaleRatToBase(d, t.Base)
	if err != nil {
		return Time{}, err
	}
	if delta > 0 && t.Ticks > math.MaxInt64-delta {
		return Time{}, ErrOverflow
	}
	if delta < 0 && t.Ticks < math.MinInt64-delta {
		return Time{}, ErrOverflow
	}
	return Time{Ticks: t.Ticks + delta, Base: t.Base}, nil
}

// Rescale converts ticks from src base to dst base, rounding to nearest.
func Rescale(ticks int64, src, dst Rational) (int64, error) {
	if !src.Valid() || !dst.Valid() || src.Num == 0 || dst.Num == 0 {
		return 0, fmt.Errorf("media: invalid rescale bases %s -> %s", src, dst)
	}
	var n, d big.Int
	n.SetInt64(ticks)
	n.Mul(&n, big.NewInt(src.Num))
	n.Mul(&n, big.NewInt(dst.Den))
	d.SetInt64(src.Den)
	d.Mul(&d, big.NewInt(dst.Num))
	q, err := divRound(&n, &d)
	if err != nil {
		return 0, err
	}
	if !q.IsInt64() {
		return 0, ErrOverflow
	}
	return q.Int64(), nil
}

func rescaleRatToBase(d Rational, base Rational) (int64, error) {
	// ticks = d / base = d.Num*base.Den / (d.Den*base.Num)
	var n, den big.Int
	n.SetInt64(d.Num)
	n.Mul(&n, big.NewInt(base.Den))
	den.SetInt64(d.Den)
	den.Mul(&den, big.NewInt(base.Num))
	q, err := divRound(&n, &den)
	if err != nil {
		return 0, err
	}
	if !q.IsInt64() {
		return 0, ErrOverflow
	}
	return q.Int64(), nil
}

// divRound divides n by d (d must be positive or negative and non-zero) and
// rounds half away from zero.
func divRound(n, d *big.Int) (*big.Int, error) {
	if d.Sign() == 0 {
		return nil, ErrZeroDenominator
	}
	q, rem := new(big.Int).QuoRem(n, d, new(big.Int))
	twice := new(big.Int).Lsh(new(big.Int).Abs(rem), 1)
	denAbs := new(big.Int).Abs(d)
	if twice.Cmp(denAbs) >= 0 {
		if n.Sign()*d.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	return q, nil
}

func ratFromBig(n, d *big.Int) (Rational, error) {
	if d.Sign() == 0 {
		return Rational{}, ErrZeroDenominator
	}
	if d.Sign() < 0 {
		n.Neg(n)
		d.Neg(d)
	}
	g := new(big.Int).GCD(nil, nil, new(big.Int).Abs(n), d)
	if g.Sign() != 0 && g.Cmp(big.NewInt(1)) != 0 {
		n.Quo(n, g)
		d.Quo(d, g)
	}
	if !n.IsInt64() || !d.IsInt64() {
		return Rational{}, ErrOverflow
	}
	return Rational{Num: n.Int64(), Den: d.Int64()}, nil
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	return a
}

func abs64(v int64) int64 {
	if v < 0 {
		if v == math.MinInt64 {
			return math.MaxInt64 // gcd only needs a magnitude bound; caller guards overflow
		}
		return -v
	}
	return v
}

func cmp64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
