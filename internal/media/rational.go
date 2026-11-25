package media

import (
	"fmt"
	"math/big"
)

// Rational is an exact signed rational number. Invariants for values returned
// by Rat: Den > 0 and gcd(|Num|, Den) == 1. The zero value behaves as 0/1 so a
// zero timestamp is directly usable.
type Rational struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

// Rat constructs a reduced rational and rejects a zero denominator or a value
// that cannot be represented in int64 after sign normalization.
func Rat(num, den int64) (Rational, *Fault) {
	if den == 0 {
		return Rational{}, NewFault(CodeInvalidTimebase, "rational denominator is zero")
	}
	return rationalFromBig(big.NewInt(num), big.NewInt(den))
}

// MustRat is for in-repo constants and deterministic fixtures only.
func MustRat(num, den int64) Rational {
	r, f := Rat(num, den)
	if f != nil {
		panic(f)
	}
	return r
}

func rationalFromBig(bn, bd *big.Int) (Rational, *Fault) {
	if bd.Sign() == 0 {
		return Rational{}, NewFault(CodeInvalidTimebase, "rational denominator is zero")
	}
	if bd.Sign() < 0 {
		bn = new(big.Int).Neg(bn)
		bd = new(big.Int).Neg(bd)
	}
	absN := new(big.Int).Abs(bn)
	g := new(big.Int).GCD(nil, nil, absN, bd)
	if g.Sign() != 0 {
		bn = new(big.Int).Quo(bn, g)
		bd = new(big.Int).Quo(bd, g)
	}
	if !bn.IsInt64() || !bd.IsInt64() {
		return Rational{}, NewFault(CodeOverflow, "rational result exceeds int64: %s/%s", bn.String(), bd.String())
	}
	return Rational{Num: bn.Int64(), Den: bd.Int64()}, nil
}

// norm maps the zero value to 0/1 and normalizes a negative denominator.
func (r Rational) norm() Rational {
	if r.Den == 0 {
		return Rational{Num: 0, Den: 1}
	}
	if r.Den < 0 {
		return Rational{Num: -r.Num, Den: -r.Den}
	}
	return r
}

// IsZero reports whether the value is zero.
func (r Rational) IsZero() bool { return r.Num == 0 }

// Float64 is intended for display/measurement only, never for accumulation.
func (r Rational) Float64() float64 {
	r = r.norm()
	return float64(r.Num) / float64(r.Den)
}

// String renders "Num/Den" (or the integer when Den == 1).
func (r Rational) String() string {
	r = r.norm()
	if r.Den == 1 {
		return fmt.Sprintf("%d", r.Num)
	}
	return fmt.Sprintf("%d/%d", r.Num, r.Den)
}

// Cmp compares two rationals exactly (no floating point). Returns -1/0/1.
func (r Rational) Cmp(o Rational) int {
	r = r.norm()
	o = o.norm()
	l := new(big.Int).Mul(big.NewInt(r.Num), big.NewInt(o.Den))
	rr := new(big.Int).Mul(big.NewInt(o.Num), big.NewInt(r.Den))
	return l.Cmp(rr)
}

// Add returns r+o as a reduced rational, rejecting int64 overflow.
func (r Rational) Add(o Rational) (Rational, *Fault) {
	r = r.norm()
	o = o.norm()
	num := new(big.Int).Add(
		new(big.Int).Mul(big.NewInt(r.Num), big.NewInt(o.Den)),
		new(big.Int).Mul(big.NewInt(o.Num), big.NewInt(r.Den)),
	)
	den := new(big.Int).Mul(big.NewInt(r.Den), big.NewInt(o.Den))
	return rationalFromBig(num, den)
}

// Sub returns r-o as a reduced rational, rejecting int64 overflow.
func (r Rational) Sub(o Rational) (Rational, *Fault) {
	r = r.norm()
	o = o.norm()
	num := new(big.Int).Sub(
		new(big.Int).Mul(big.NewInt(r.Num), big.NewInt(o.Den)),
		new(big.Int).Mul(big.NewInt(o.Num), big.NewInt(r.Den)),
	)
	den := new(big.Int).Mul(big.NewInt(r.Den), big.NewInt(o.Den))
	return rationalFromBig(num, den)
}
