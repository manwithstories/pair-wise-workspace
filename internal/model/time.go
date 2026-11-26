package model

import (
	"fmt"
	"math/big"
)

// Rational is an exact signed rational time value. It is never converted
// through float64, so cumulative remux timing remains deterministic.
type Rational struct {
	N int64 `json:"n"`
	D int64 `json:"d"`
}

// NewRational returns a normalized rational. A zero denominator is rejected.
func NewRational(n, d int64) (Rational, error) {
	if d == 0 {
		return Rational{}, fmt.Errorf("rational denominator is zero")
	}
	if d < 0 {
		n, d = -n, -d
	}
	g := gcd64(abs64(n), d)
	return Rational{N: n / g, D: d / g}, nil
}

// MustRational is intended for compile-time-known valid ratios in tests and
// constants.
func MustRational(n, d int64) Rational {
	r, err := NewRational(n, d)
	if err != nil {
		panic(err)
	}
	return r
}

// Ticks represents value/timeScale seconds as an exact rational.
func Ticks(value int64, timeScale uint32) Rational {
	if timeScale == 0 {
		return Rational{N: 0, D: 1}
	}
	r, _ := NewRational(value, int64(timeScale))
	return r
}

// ConvertTicks converts source ticks to an exact rational expressed in
// fromScale units and then scales the ratio by toScale. The result is rational;
// use ExactTicks if an integral output sample count is required.
func ConvertTicks(value int64, fromScale, toScale uint32) (Rational, error) {
	if fromScale == 0 || toScale == 0 {
		return Rational{}, fmt.Errorf("time scale must be positive")
	}
	return NewRational(value*int64(toScale), int64(fromScale))
}

// ExactTicks reports r as an integral number of ticks at scale.
func (r Rational) ExactTicks(scale uint32) (int64, bool) {
	if scale == 0 || r.D == 0 {
		return 0, false
	}
	num := new(big.Int).Mul(big.NewInt(r.N), big.NewInt(int64(scale)))
	den := big.NewInt(r.D)
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	if rem.Sign() != 0 || !q.IsInt64() {
		return 0, false
	}
	return q.Int64(), true
}

// Cmp compares two rationals without floating-point arithmetic.
func (r Rational) Cmp(o Rational) int {
	if r.D == 0 || o.D == 0 {
		panic("compare invalid rational")
	}
	l := new(big.Int).Mul(big.NewInt(r.N), big.NewInt(o.D))
	rr := new(big.Int).Mul(big.NewInt(o.N), big.NewInt(r.D))
	return l.Cmp(rr)
}

func (r Rational) String() string {
	if r.D == 1 {
		return fmt.Sprintf("%d", r.N)
	}
	return fmt.Sprintf("%d/%d", r.N, r.D)
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func gcd64(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		return 1
	}
	return a
}
