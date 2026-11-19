// Package model defines the shared timeline vocabulary: a rational time base,
// tracks, clips, crossfades and locatable faults. All times are integer
// rationals so that no floating point accumulation is possible.
package model

import (
	"math"
	"math/big"
	"math/bits"
)

// Rational is a normalized integer fraction. The zero value is 0/0 which is
// treated as an invalid fraction by Validate; construct with NewRat.
type Rational struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

// NewRat returns num/den in lowest terms. den must be positive; a negative
// denominator flips the sign.
func NewRat(num, den int64) Rational {
	if den == 0 {
		return Rational{}
	}
	if den < 0 {
		num, den = -num, -den
	}
	g := gcd(abs(num), den)
	return Rational{Num: num / g, Den: den / g}
}

// Valid reports whether the fraction has a positive denominator.
func (r Rational) Valid() bool { return r.Den > 0 }

// Sign returns -1, 0 or 1.
func (r Rational) Sign() int {
	switch {
	case !r.Valid() || r.Num == 0:
		return 0
	case r.Num > 0:
		return 1
	default:
		return -1
	}
}

// Add returns r+o.
func (r Rational) Add(o Rational) Rational {
	if !r.Valid() || !o.Valid() {
		return Rational{}
	}
	if n, ok := addFrac(r, o); ok {
		return n
	}
	return addBig(r, o)
}

// Sub returns r-o.
func (r Rational) Sub(o Rational) Rational { return r.Add(o.Neg()) }

// Neg returns -r.
func (r Rational) Neg() Rational { return Rational{Num: -r.Num, Den: r.Den} }

// Mul returns r*o.
func (r Rational) Mul(o Rational) Rational {
	if !r.Valid() || !o.Valid() {
		return Rational{}
	}
	if n, ok := mulFrac(r, o); ok {
		return n
	}
	return mulBig(r, o)
}

// Div returns r/o.
func (r Rational) Div(o Rational) Rational {
	if !r.Valid() || !o.Valid() || o.Num == 0 {
		return Rational{}
	}
	return r.Mul(Rational{Num: o.Den, Den: o.Num})
}

// Cmp compares r and o returning -1, 0 or 1.
func (r Rational) Cmp(o Rational) int {
	if !r.Valid() || !o.Valid() {
		return 0
	}
	a := new(big.Int).Mul(big.NewInt(r.Num), big.NewInt(o.Den))
	b := new(big.Int).Mul(big.NewInt(o.Num), big.NewInt(r.Den))
	return a.Cmp(b)
}

// Scale converts the fraction to ticks of the given timescale, rounding to
// nearest (ties away from zero).
func (r Rational) Scale(timescale uint32) int64 {
	if !r.Valid() || timescale == 0 {
		return 0
	}
	n := new(big.Int).Mul(big.NewInt(r.Num), new(big.Int).SetUint64(uint64(timescale)))
	d := big.NewInt(r.Den)
	q, rem := new(big.Int).QuoRem(n, d, new(big.Int))
	// rem and d share sign; round half away from zero.
	twice := new(big.Int).Abs(rem)
	twice.Lsh(twice, 1)
	if twice.Cmp(new(big.Int).Abs(d)) >= 0 {
		if (n.Sign() < 0) != (d.Sign() < 0) {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	if !q.IsInt64() {
		return 0
	}
	return q.Int64()
}

// Millis returns the duration rounded to whole milliseconds.
func (r Rational) Millis() int64 { return r.Scale(1000) }

// Float64 is only used for reporting; internal time never flows through it.
func (r Rational) Float64() float64 {
	if !r.Valid() {
		return 0
	}
	rat := bigRat(r)
	f, _ := rat.Float64()
	return f
}

// String renders the fraction as "num/den".
func (r Rational) String() string {
	if !r.Valid() {
		return "invalid"
	}
	return bigRat(r).RatString()
}

// FromTicks converts ticks at timescale to a fraction of a second.
func FromTicks(ticks int64, timescale uint32) Rational {
	return NewRat(ticks, int64(timescale))
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

func abs(a int64) int64 {
	if a < 0 {
		return -a
	}
	return a
}

func bigRat(r Rational) *big.Rat {
	return big.NewRat(r.Num, r.Den)
}

func ratOf(z *big.Rat) Rational {
	if !z.Num().IsInt64() || !z.Denom().IsInt64() {
		// Saturate rather than silently truncating; callers that care about
		// representability use the int64 fast path first.
		f, _ := z.Float64()
		if f < 0 {
			return NewRat(-math.MaxInt64, 1)
		}
		return NewRat(math.MaxInt64, 1)
	}
	return NewRat(z.Num().Int64(), z.Denom().Int64())
}

// addBig returns r+o using arbitrary precision.
func addBig(r, o Rational) Rational {
	var z big.Rat
	z.Add(bigRat(r), bigRat(o))
	return ratOf(&z)
}

// mulBig returns r*o using arbitrary precision (only denominators that are not
// int64-representable take this path).
func mulBig(r, o Rational) Rational {
	var z big.Rat
	z.Mul(bigRat(r), bigRat(o))
	return ratOf(&z)
}

// mulInt64 multiplies with overflow detection.
func mulInt64(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	neg := (a < 0) != (b < 0)
	hi, lo := bits.Mul64(uint64(abs(a)), uint64(abs(b)))
	if hi != 0 {
		return 0, false
	}
	limit := uint64(math.MaxInt64)
	if neg {
		limit++
	}
	if lo > limit {
		return 0, false
	}
	if neg {
		return -int64(lo), true
	}
	return int64(lo), true
}

func addInt64(a, b int64) (int64, bool) {
	s := a + b
	if (a > 0 && b > 0 && s < 0) || (a < 0 && b < 0 && s >= 0) {
		return 0, false
	}
	return s, true
}

func mulFrac(a, b Rational) (Rational, bool) {
	n, ok := mulInt64(a.Num, b.Num)
	if !ok {
		return Rational{}, false
	}
	d, ok := mulInt64(a.Den, b.Den)
	if !ok {
		return Rational{}, false
	}
	return NewRat(n, d), true
}

func addFrac(a, b Rational) (Rational, bool) {
	p1, ok1 := mulInt64(a.Num, b.Den)
	p2, ok2 := mulInt64(b.Num, a.Den)
	if !ok1 || !ok2 {
		return Rational{}, false
	}
	n, ok := addInt64(p1, p2)
	if !ok {
		return Rational{}, false
	}
	d, ok := mulInt64(a.Den, b.Den)
	if !ok {
		return Rational{}, false
	}
	return NewRat(n, d), true
}
