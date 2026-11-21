// Package clock provides an exact, overflow-checked rational time base.
//
// Every media timestamp in mediad is represented as seconds in the form
// num/den. Keeping the value exact avoids the cumulative drift that shows up
// when millisecond or microsecond integers are used to describe a 30000/1001
// frame duration.
package clock

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Errors returned by the rational time base.
var (
	ErrZeroDenominator = errors.New("clock: zero denominator")
	ErrOverflow        = errors.New("clock: rational overflow")
)

// Rational is an exact rational number. The zero value is 0/1. The
// denominator is always positive and the fraction is always reduced.
type Rational struct {
	Num int64
	Den int64
}

// New returns num/den in canonical form.
func New(num, den int64) (Rational, error) {
	return canon(num, den)
}

// MustNew is New for constants known to be valid.
func MustNew(num, den int64) Rational {
	r, err := New(num, den)
	if err != nil {
		panic(err)
	}
	return r
}

// Zero is the additive identity, 0 seconds.
func Zero() Rational { return Rational{Num: 0, Den: 1} }

// FromInt is the whole number n in seconds.
func FromInt(n int64) Rational { return Rational{Num: n, Den: 1} }

// FromMillis returns ms milliseconds as an exact fraction of a second.
func FromMillis(ms int64) (Rational, error) { return New(ms, 1000) }

// FromMicros returns us microseconds as an exact fraction of a second.
func FromMicros(us int64) (Rational, error) { return New(us, 1_000_000) }

// FromTicks converts a tick count at timescale ticks/second.
func FromTicks(ticks, timescale int64) (Rational, error) {
	if timescale <= 0 {
		return Rational{}, fmt.Errorf("clock: invalid timescale %d: %w", timescale, ErrZeroDenominator)
	}
	return New(ticks, timescale)
}

// Parse accepts "n/d", a plain integer or a decimal such as "23.976".
func Parse(s string) (Rational, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Rational{}, errors.New("clock: empty rational")
	}
	if strings.Contains(s, "/") {
		parts := strings.Split(s, "/")
		if len(parts) != 2 {
			return Rational{}, fmt.Errorf("clock: malformed rational %q", s)
		}
		n, err1 := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		d, err2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err1 != nil || err2 != nil {
			return Rational{}, fmt.Errorf("clock: malformed rational %q", s)
		}
		return New(n, d)
	}
	x, ok := new(big.Rat).SetString(s)
	if !ok {
		return Rational{}, fmt.Errorf("clock: malformed decimal %q", s)
	}
	return fromBig(x)
}

// IsZero reports whether r is exactly zero.
func (r Rational) IsZero() bool { return r.Num == 0 }

// Sign returns -1, 0 or +1.
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

// Neg returns -r.
func (r Rational) Neg() Rational {
	c := r.canonical()
	if c.Num == math.MinInt64 {
		n, err := fromBig(new(big.Rat).Neg(c.big()))
		if err != nil {
			return Rational{}
		}
		return n
	}
	return Rational{Num: -c.Num, Den: c.Den}
}

// Abs returns |r|.
func (r Rational) Abs() Rational {
	if r.Sign() < 0 {
		return r.Neg()
	}
	return r.canonical()
}

// Add returns r+o, reporting overflow of the int64 representation.
func (r Rational) Add(o Rational) (Rational, error) {
	return fromBig(new(big.Rat).Add(r.big(), o.big()))
}

// Sub returns r-o.
func (r Rational) Sub(o Rational) (Rational, error) {
	return fromBig(new(big.Rat).Sub(r.big(), o.big()))
}

// Mul returns r*o.
func (r Rational) Mul(o Rational) (Rational, error) {
	return fromBig(new(big.Rat).Mul(r.big(), o.big()))
}

// Div returns r/o.
func (r Rational) Div(o Rational) (Rational, error) {
	if o.IsZero() {
		return Rational{}, ErrZeroDenominator
	}
	return fromBig(new(big.Rat).Quo(r.big(), o.big()))
}

// MulInt multiplies by an integer.
func (r Rational) MulInt(n int64) (Rational, error) {
	return fromBig(new(big.Rat).Mul(r.big(), new(big.Rat).SetInt64(n)))
}

// Cmp compares r and o, returning -1, 0 or +1.
func (r Rational) Cmp(o Rational) int { return r.big().Cmp(o.big()) }

// Equal reports exact equality.
func (r Rational) Equal(o Rational) bool { return r.Cmp(o) == 0 }

// Less reports r < o.
func (r Rational) Less(o Rational) bool { return r.Cmp(o) < 0 }

// Float64 returns the nearest float64; use only for display, never for arithmetic.
func (r Rational) Float64() float64 {
	c := r.canonical()
	return float64(c.Num) / float64(c.Den)
}

// Milliseconds returns r in milliseconds as a float for reporting.
func (r Rational) Milliseconds() float64 { return r.Float64() * 1000 }

// Trunc returns the value truncated toward zero.
func (r Rational) Trunc() int64 {
	c := r.canonical()
	return c.Num / c.Den
}

// Round returns the nearest integer, halves away from zero.
func (r Rational) Round() int64 {
	c := r.canonical()
	q := c.Num / c.Den
	rem := c.Num % c.Den
	if rem == 0 {
		return q
	}
	absRem := rem
	if absRem < 0 {
		absRem = -absRem
	}
	// absRem*2 >= c.Den, written to avoid overflow.
	if absRem >= c.Den-absRem {
		if c.Num > 0 {
			q++
		} else {
			q--
		}
	}
	return q
}

// Rescale converts seconds into some unit U where one U is 1/unitsPerSecond
// seconds, rounding to nearest.
func (r Rational) Rescale(unitsPerSecond int64) (int64, error) {
	if unitsPerSecond <= 0 {
		return 0, fmt.Errorf("clock: invalid rescale base %d", unitsPerSecond)
	}
	x := new(big.Rat).Mul(r.big(), new(big.Rat).SetInt64(unitsPerSecond))
	num, den := x.Num(), x.Denom()
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	if new(big.Int).Lsh(new(big.Int).Abs(rem), 1).Cmp(den) >= 0 {
		if num.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	if !q.IsInt64() {
		return 0, ErrOverflow
	}
	return q.Int64(), nil
}

// String renders the exact fraction, or a plain integer when integral.
func (r Rational) String() string {
	c := r.canonical()
	if c.Den == 1 {
		return strconv.FormatInt(c.Num, 10)
	}
	return strconv.FormatInt(c.Num, 10) + "/" + strconv.FormatInt(c.Den, 10)
}

// UnmarshalJSON decodes the exact string form written by MarshalJSON.
func (r *Rational) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	v, err := Parse(s)
	if err != nil {
		return err
	}
	*r = v
	return nil
}

// MarshalJSON encodes the value as its exact string form.
func (r Rational) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(r.String())), nil
}

func (r Rational) canonical() Rational {
	c, err := canon(r.Num, r.Den)
	if err != nil {
		return Rational{Num: r.Num, Den: 1}
	}
	return c
}

func (r Rational) big() *big.Rat {
	c := r.canonical()
	return new(big.Rat).SetFrac64(c.Num, c.Den)
}

func canon(num, den int64) (Rational, error) {
	if den == 0 {
		return Rational{}, ErrZeroDenominator
	}
	if num == 0 {
		return Rational{Num: 0, Den: 1}, nil
	}
	neg := (num < 0) != (den < 0)
	an, ad := absU(num), absU(den)
	g := gcd(an, ad)
	an, ad = an/g, ad/g
	if ad > uint64(math.MaxInt64) {
		// Only reachable when den == MinInt64; fall back to exact big math.
		n, err := fromBig(new(big.Rat).SetFrac64(num, den))
		if err != nil {
			return Rational{}, err
		}
		return n, nil
	}
	if an > uint64(math.MaxInt64) {
		return Rational{}, ErrOverflow
	}
	n := int64(an)
	if neg {
		if an == 1<<63 {
			n = math.MinInt64
		} else {
			n = -int64(an)
		}
	}
	return Rational{Num: n, Den: int64(ad)}, nil
}

func absU(x int64) uint64 {
	if x < 0 {
		return uint64(-(x + 1)) + 1
	}
	return uint64(x)
}

func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func fromBig(x *big.Rat) (Rational, error) {
	n, d := x.Num(), x.Denom()
	if !n.IsInt64() || !d.IsInt64() {
		return Rational{}, ErrOverflow
	}
	return canon(n.Int64(), d.Int64())
}
