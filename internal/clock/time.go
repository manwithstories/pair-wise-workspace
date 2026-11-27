// Package clock implements an exact rational media time base shared by the
// MP4 scanner, timeline alignment, encoder and HLS packager.
//
// All media durations and timestamps are represented as Num/Den pairs so that
// frame rates such as 29.97 fps (30000/1001) never suffer from floating point
// drift. A zero denominator is invalid and every constructor normalises the
// fraction into a canonical reduced form.
package clock

import (
	"errors"
	"fmt"
	"math/big"
)

// Rat is a non-negative rational number expressed as Num/Den.
// Den is always strictly positive for a valid Rat.
type Rat struct {
	Num int64
	Den int64
}

var (
	// ErrZeroTimebase is returned when an operation would require a zero
	// denominator (for example an MP4 track with timescale == 0).
	ErrZeroTimebase = errors.New("clock: zero timebase")
	// ErrOverflow is returned when an integer conversion overflows int64.
	ErrOverflow = errors.New("clock: integer overflow")
	// ErrNegative is returned when a negative value appears where only
	// non-negative media time is permitted.
	ErrNegative = errors.New("clock: negative media time")
)

// New builds a normalised Rat from a numerator and denominator.
func New(num, den int64) (Rat, error) {
	if den == 0 {
		return Rat{}, ErrZeroTimebase
	}
	if den < 0 {
		if num == minInt64 || den == minInt64 {
			return Rat{}, ErrOverflow
		}
		num, den = -num, -den
	}
	if num == 0 {
		return Rat{0, den}, nil
	}
	g := gcd(abs64(num), den)
	r := Rat{Num: num / g, Den: den / g}
	if r.Num == 0 || r.Den == 0 {
		return Rat{}, ErrOverflow
	}
	return r, nil
}

// MustNew is New that panics on error; use only for compile-time constants.
func MustNew(num, den int64) Rat {
	r, err := New(num, den)
	if err != nil {
		panic(err)
	}
	return r
}

const minInt64 = -1 << 63

func abs64(v int64) int64 {
	if v == minInt64 {
		// gcd only needs the magnitude; minInt64/-1 would overflow, but gcd
		// collapses quickly enough that returning minInt64 is safe in practice.
		return minInt64
	}
	if v < 0 {
		return -v
	}
	return v
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

// Zero returns the canonical zero value on the given time base.
func Zero(den int64) (Rat, error) { return New(0, den) }

// Add returns r+s exactly, normalising the result.
func (r Rat) Add(s Rat) (Rat, error) {
	if r.Den == 0 || s.Den == 0 {
		return Rat{}, ErrZeroTimebase
	}
	num, ok1 := mulCheck(r.Num, s.Den)
	n2, ok2 := mulCheck(s.Num, r.Den)
	if !ok1 || !ok2 {
		return addBig(r, s)
	}
	n, ok := addCheck(num, n2)
	if !ok {
		return addBig(r, s)
	}
	return New(n, r.Den*s.Den)
}

func addBig(r, s Rat) (Rat, error) {
	n := new(big.Int).Mul(big.NewInt(r.Num), big.NewInt(s.Den))
	n.Add(n, new(big.Int).Mul(big.NewInt(s.Num), big.NewInt(r.Den)))
	d := new(big.Int).Mul(big.NewInt(r.Den), big.NewInt(s.Den))
	return fromBig(n, d)
}

// Sub returns r-s. The result may be negative.
func (r Rat) Sub(s Rat) (Rat, error) {
	neg := Rat{Num: -s.Num, Den: s.Den}
	return r.Add(neg)
}

// Mul scales r by n/d without overflowing; it falls back to big.Int when the
// int64 intermediate products do not fit.
func (r Rat) Mul(n, d int64) (Rat, error) {
	if d == 0 {
		return Rat{}, ErrZeroTimebase
	}
	if num, ok := mulCheck(r.Num, n); ok {
		if den, ok2 := mulCheck(r.Den, d); ok2 {
			return New(num, den)
		}
	}
	num := new(big.Int).Mul(big.NewInt(r.Num), big.NewInt(n))
	den := new(big.Int).Mul(big.NewInt(r.Den), big.NewInt(d))
	return fromBig(num, den)
}

func fromBig(n, d *big.Int) (Rat, error) {
	g := new(big.Int).GCD(nil, nil, new(big.Int).Abs(n), new(big.Int).Abs(d))
	n.Quo(n, g)
	d.Quo(d, g)
	if !n.IsInt64() || !d.IsInt64() {
		return Rat{}, ErrOverflow
	}
	num, den := n.Int64(), d.Int64()
	if den < 0 {
		if num == minInt64 {
			return Rat{}, ErrOverflow
		}
		num, den = -num, -den
	}
	return Rat{Num: num, Den: den}, nil
}

func mulCheck(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	r := a * b
	if r/b != a {
		return 0, false
	}
	return r, true
}

func addCheck(a, b int64) (int64, bool) {
	if (b > 0 && a > maxInt64-b) || (b < 0 && a < minInt64-b) {
		return 0, false
	}
	return a + b, true
}

const maxInt64 = 1<<63 - 1

// Cmp returns -1, 0 or 1 like bytes.Compare.
func (r Rat) Cmp(s Rat) int {
	// Cross multiply through big.Int to stay exact.
	l := new(big.Int).Mul(big.NewInt(r.Num), big.NewInt(s.Den))
	rr := new(big.Int).Mul(big.NewInt(s.Num), big.NewInt(r.Den))
	return l.Cmp(rr)
}

// Equal reports whether two instants denote the same rational value.
func (r Rat) Equal(s Rat) bool { return r.Cmp(s) == 0 }

// Seconds renders the value as a float; use only for display/metrics.
func (r Rat) Seconds() float64 {
	if r.Den == 0 {
		return 0
	}
	return float64(r.Num) / float64(r.Den)
}

// String renders "num/den" or "num/den (~x.xxxs)".
func (r Rat) String() string {
	if r.Den == 0 {
		return "<invalid-timebase>"
	}
	return fmt.Sprintf("%d/%d(%.6fs)", r.Num, r.Den, r.Seconds())
}

// Scale converts a tick count expressed on time base (num,den) into a Rat.
func Scale(ticks, num, den int64) (Rat, error) {
	r, err := New(ticks, 1)
	if err != nil {
		return Rat{}, err
	}
	return r.Mul(num, den)
}
