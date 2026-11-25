// Package clock defines the exact rational time base shared by the
// manifest parser, fragment scanner, repair engine, indexer, executor
// and independent auditor.
//
// All inter-module timestamps are exchanged either as raw ticks in a
// declared timescale or as Rational numbers of seconds. Floating point
// is only used at the metric/reporting boundary.
package clock

import (
	"fmt"
	"math/big"
)

// Rational is an exact N/D rational number of seconds with D > 0.
// The zero value is treated as 0/1 by the arithmetic helpers.
type Rational struct {
	N int64
	D int64
}

// New returns the normalized rational n/d. It panics on a zero
// denominator because that is a programming error, not input damage.
func New(n, d int64) Rational {
	if d == 0 {
		panic("clock: rational with zero denominator")
	}
	if d < 0 {
		n, d = -n, -d
	}
	g := gcd(abs64(n), d)
	if g > 1 {
		n /= g
		d /= g
	}
	return Rational{N: n, D: d}
}

// Zero is the zero point on a media timeline.
func Zero() Rational { return Rational{D: 1} }

func (r Rational) den() int64 {
	if r.D == 0 {
		return 1
	}
	return r.D
}

// Cmp returns -1 if r < o, 0 if equal and +1 if r > o.
func (r Rational) Cmp(o Rational) int {
	d := r.den()
	od := o.den()
	lhs := new(big.Int).Mul(big.NewInt(r.N), big.NewInt(od))
	rhs := new(big.Int).Mul(big.NewInt(o.N), big.NewInt(d))
	return lhs.Cmp(rhs)
}

// Add returns r+o as a normalized rational.
func (r Rational) Add(o Rational) Rational {
	d := r.den()
	od := o.den()
	n := new(big.Int).Add(
		new(big.Int).Mul(big.NewInt(r.N), big.NewInt(od)),
		new(big.Int).Mul(big.NewInt(o.N), big.NewInt(d)),
	)
	den := new(big.Int).Mul(big.NewInt(d), big.NewInt(od))
	return fromBig(n, den)
}

// Sub returns r-o as a normalized rational.
func (r Rational) Sub(o Rational) Rational {
	d := r.den()
	od := o.den()
	n := new(big.Int).Sub(
		new(big.Int).Mul(big.NewInt(r.N), big.NewInt(od)),
		new(big.Int).Mul(big.NewInt(o.N), big.NewInt(d)),
	)
	den := new(big.Int).Mul(big.NewInt(d), big.NewInt(od))
	return fromBig(n, den)
}

// MulInt64 returns r*k.
func (r Rational) MulInt64(k int64) Rational {
	return New(r.N*k, r.den())
}

// ToMillis converts the rational seconds value to float milliseconds.
func (r Rational) ToMillis() float64 {
	f, _ := new(big.Rat).SetFrac(big.NewInt(r.N), big.NewInt(r.den())).Float64()
	return f * 1000.0
}

// MillisDiff returns |a-b| expressed in milliseconds.
func MillisDiff(a, b Rational) float64 {
	d := a.Sub(b)
	if d.N < 0 {
		d.N = -d.N
	}
	return d.ToMillis()
}

// String renders the rational like "9001/90000".
func (r Rational) String() string {
	return fmt.Sprintf("%d/%d", r.N, r.den())
}

// Timebase is a media timescale: Timescale ticks elapse per second.
type Timebase struct {
	Timescale uint32
}

// Rational converts ticks in this timescale to exact seconds.
func (tb Timebase) Rational(ticks int64) Rational {
	return New(ticks, int64(tb.Timescale))
}

// Millis converts ticks to floating point milliseconds.
func (tb Timebase) Millis(ticks int64) float64 {
	return tb.Rational(ticks).ToMillis()
}

// CommonTimescale is a timescale into which both 48 kHz audio and
// 90 kHz video timestamps rescale without any rounding:
// lcm(48000,90000) = 720000.
const CommonTimescale uint32 = 720000

// RescaleExact converts ticks from one timescale to another only when
// the conversion is integral. The boolean reports exactness.
func RescaleExact(ticks int64, from, to uint32) (int64, bool) {
	if from == 0 || to == 0 {
		return 0, false
	}
	num := new(big.Int).Mul(big.NewInt(ticks), big.NewInt(int64(to)))
	den := big.NewInt(int64(from))
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	if rem.Sign() != 0 || !q.IsInt64() {
		return 0, false
	}
	return q.Int64(), true
}

// RescaleRound converts ticks between timescales using nearest
// rounding. Callers that need exact AV sync should use RescaleExact
// with CommonTimescale instead.
func RescaleRound(ticks int64, from, to uint32) int64 {
	if from == 0 {
		return 0
	}
	num := new(big.Int).Mul(big.NewInt(ticks), big.NewInt(int64(to)))
	den := big.NewInt(int64(from))
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	twoRem := new(big.Int).Lsh(rem, 1)
	if twoRem.Cmp(den) >= 0 {
		q.Add(q, big.NewInt(sign(ticks)))
	}
	return q.Int64()
}

func fromBig(n, d *big.Int) Rational {
	if d.Sign() < 0 {
		n.Neg(n)
		d.Neg(d)
	}
	g := new(big.Int).GCD(nil, nil, new(big.Int).Abs(n), d)
	if g.Sign() > 0 {
		n.Quo(n, g)
		d.Quo(d, g)
	}
	return Rational{N: n.Int64(), D: d.Int64()}
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func sign(v int64) int64 {
	if v < 0 {
		return -1
	}
	return 1
}
