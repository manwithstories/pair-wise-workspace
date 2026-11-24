// Package media contains the shared rational time model and graded errors
// used by capture, audio, MP4, transaction, and reporting components.
package media

import (
	"errors"
	"fmt"
	"math/big"
)

// Rational is an exact signed fraction. Duration arithmetic in mediarig is
// performed with integers or Rational values; float64 is only used at output
// boundaries.
type Rational struct {
	N int64
	D int64
}

// Rat returns a normalized rational. A zero denominator is rejected rather
// than silently producing an infinite clock.
func Rat(n, d int64) (Rational, error) {
	if d == 0 {
		return Rational{}, errors.New("media: zero rational denominator")
	}
	if d < 0 {
		n, d = -n, -d
	}
	g := gcd64(abs64(n), uint64(d))
	if g != 0 {
		n /= int64(g)
		d /= int64(g)
	}
	return Rational{N: n, D: d}, nil
}

func MustRat(n, d int64) Rational {
	r, err := Rat(n, d)
	if err != nil {
		panic(err)
	}
	return r
}

func gcd64(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func abs64(x int64) uint64 {
	if x < 0 {
		return uint64(-x)
	}
	return uint64(x)
}

func (r Rational) IsZero() bool { return r.N == 0 }

func (r Rational) Add(o Rational) Rational {
	n := big.NewInt(r.N)
	d := big.NewInt(r.D)
	on := big.NewInt(o.N)
	od := big.NewInt(o.D)
	num := new(big.Int).Mul(n, od)
	num.Add(num, new(big.Int).Mul(on, d))
	den := new(big.Int).Mul(d, od)
	rr, _ := Rat(num.Int64(), den.Int64())
	return rr
}

func (r Rational) Sub(o Rational) Rational {
	return r.Add(Rational{N: -o.N, D: o.D})
}

func (r Rational) MulInt64(n int64) Rational {
	rr, _ := Rat(r.N*n, r.D)
	return rr
}

// MulRat multiplies by another rational using arbitrary-size intermediates so
// ordinary batch durations cannot overflow before normalization.
func (r Rational) MulRat(o Rational) Rational {
	n := new(big.Int).Mul(big.NewInt(r.N), big.NewInt(o.N))
	d := new(big.Int).Mul(big.NewInt(r.D), big.NewInt(o.D))
	return bigRatToRational(n, d)
}

func bigRatToRational(n, d *big.Int) Rational {
	if d.Sign() < 0 {
		n = new(big.Int).Neg(n)
		d = new(big.Int).Neg(d)
	}
	g := new(big.Int).GCD(nil, nil, new(big.Int).Abs(n), new(big.Int).Abs(d))
	if g.Sign() != 0 && g.Cmp(big.NewInt(1)) != 0 {
		n.Quo(n, g)
		d.Quo(d, g)
	}
	if !n.IsInt64() || !d.IsInt64() {
		// Media durations in this bounded CLI remain int64-sized. Keeping the
		// fallback explicit avoids silent wraparound.
		panic("media: rational exceeds int64 after reduction")
	}
	return Rational{N: n.Int64(), D: d.Int64()}
}

func (r Rational) Cmp(o Rational) int {
	l := new(big.Int).Mul(big.NewInt(r.N), big.NewInt(o.D))
	rr := new(big.Int).Mul(big.NewInt(o.N), big.NewInt(r.D))
	return l.Cmp(rr)
}

func (r Rational) Float64() float64 {
	return float64(r.N) / float64(r.D)
}

func (r Rational) String() string {
	if r.D == 1 {
		return fmt.Sprintf("%d", r.N)
	}
	return fmt.Sprintf("%d/%d", r.N, r.D)
}

// Seconds is a semantic alias for Rational values measured in seconds.
func Seconds(n, d int64) Rational { return MustRat(n, d) }

// SamplesToSeconds maps a tick count at rate ticks/second to exact seconds.
func SamplesToSeconds(samples int64, rate int) Rational {
	return MustRat(samples, int64(rate))
}

// SecondsToSamples rounds an exact second value to the nearest integer tick.
func SecondsToSamples(t Rational, rate int) int64 {
	num := new(big.Int).Mul(big.NewInt(t.N), big.NewInt(int64(rate)))
	den := big.NewInt(t.D)
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	if rem.Sign() < 0 {
		rem.Add(rem, den)
		q.Sub(q, big.NewInt(1))
	}
	twice := new(big.Int).Mul(rem, big.NewInt(2))
	if twice.Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	return q.Int64()
}

// MaxSkewSamples returns the absolute mapping error between two media times in
// units of the supplied sample rate.
func MaxSkewSamples(a, b Rational, rate int) Rational {
	d := a.Sub(b)
	if d.N < 0 {
		d.N = -d.N
	}
	return d.MulRat(MustRat(int64(rate), 1))
}
