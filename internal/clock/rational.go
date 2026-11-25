// Package clock provides overflow-checked rational time bases and timeline assembly.
package clock

import (
	"math/big"

	"mediad/internal/fault"
)

// Rational is a canonical integer rational with a positive denominator.
type Rational struct {
	N int64 `json:"n"`
	D int64 `json:"d"`
}

var (
	Zero = Rational{N: 0, D: 1}
	One  = Rational{N: 1, D: 1}
)

func canonical(n, d *big.Int) (Rational, error) {
	if d.Sign() == 0 {
		return Rational{}, fault.New(fault.TimestampOverflow, "", -1, "zero time denominator")
	}
	if d.Sign() < 0 {
		n.Neg(n)
		d.Neg(d)
	}
	a := new(big.Int).Abs(n)
	g := new(big.Int).GCD(nil, nil, a, d)
	if g.Sign() > 0 {
		n.Quo(n, g)
		d.Quo(d, g)
	}
	if !n.IsInt64() || !d.IsInt64() {
		return Rational{}, fault.New(fault.TimestampOverflow, "", -1, "rational exceeds int64 after reduction")
	}
	return Rational{N: n.Int64(), D: d.Int64()}, nil
}

// New returns a canonical rational or an overflow/location error.
func New(n, d int64) (Rational, error) {
	return canonical(big.NewInt(n), big.NewInt(d))
}

// Must is for compile-time-known valid fixture constants.
func Must(n, d int64) Rational {
	r, err := New(n, d)
	if err != nil {
		panic(err)
	}
	return r
}

func (r Rational) big() (*big.Int, *big.Int) {
	return big.NewInt(r.N), big.NewInt(r.D)
}

func (r Rational) IsZero() bool { return r.N == 0 }

func (r Rational) Float64() float64 { return float64(r.N) / float64(r.D) }

func (r Rational) String() string {
	if r.D == 1 {
		return big.NewInt(r.N).String()
	}
	return big.NewInt(r.N).String() + "/" + big.NewInt(r.D).String()
}

// Add returns a+b while rejecting numerator/denominator overflow.
func Add(a, b Rational) (Rational, error) {
	an, ad := a.big()
	bn, bd := b.big()
	n := new(big.Int).Add(new(big.Int).Mul(an, bd), new(big.Int).Mul(bn, ad))
	d := new(big.Int).Mul(ad, bd)
	return canonical(n, d)
}

// Sub returns a-b while rejecting overflow.
func Sub(a, b Rational) (Rational, error) {
	an, ad := a.big()
	bn, bd := b.big()
	n := new(big.Int).Sub(new(big.Int).Mul(an, bd), new(big.Int).Mul(bn, ad))
	d := new(big.Int).Mul(ad, bd)
	return canonical(n, d)
}

// Mul returns a*b while rejecting overflow.
func Mul(a, b Rational) (Rational, error) {
	an, ad := a.big()
	bn, bd := b.big()
	return canonical(new(big.Int).Mul(an, bn), new(big.Int).Mul(ad, bd))
}

// Cmp returns -1, 0, or 1. It uses arbitrary precision cross products.
func Cmp(a, b Rational) int {
	an, ad := a.big()
	bn, bd := b.big()
	l := new(big.Int).Mul(an, bd)
	r := new(big.Int).Mul(bn, ad)
	return l.Cmp(r)
}

// SamplesToTime maps integral samples at rate Hz to seconds.
func SamplesToTime(samples, rate int64) (Rational, error) {
	if rate <= 0 {
		return Rational{}, fault.New(fault.TimestampOverflow, "", -1, "non-positive sample rate %d", rate)
	}
	return New(samples, rate)
}

// Ticks maps seconds to an integral number of clock ticks. The time must be
// exactly representable on the requested clock.
func Ticks(t Rational, rate int64) (int64, error) {
	if rate <= 0 {
		return 0, fault.New(fault.TimestampOverflow, "", -1, "non-positive tick rate %d", rate)
	}
	n, d := t.big()
	n.Mul(n, big.NewInt(rate))
	q, rem := new(big.Int).QuoRem(n, d, new(big.Int))
	if rem.Sign() != 0 || !q.IsInt64() {
		return 0, fault.New(fault.TimestampOverflow, "", -1, "time %s is not integral at %d Hz", t, rate)
	}
	return q.Int64(), nil
}

// TicksRounded maps seconds to nearest clock tick, used by RTP packetization.
func TicksRounded(t Rational, rate int64) (int64, error) {
	exact, err := Ticks(t, rate)
	if err == nil {
		return exact, nil
	}
	n, d := t.big()
	num := new(big.Int).Mul(n, big.NewInt(rate))
	den := d
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	if twice := new(big.Int).Mul(rem, big.NewInt(2)); twice.Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, fault.New(fault.TimestampOverflow, "", -1, "rounded tick overflow at %d Hz", rate)
	}
	return q.Int64(), nil
}
