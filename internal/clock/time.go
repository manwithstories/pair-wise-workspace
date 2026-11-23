// Package clock provides the single rational time base and rounding rules
// used by every subsystem. All timeline arithmetic that crosses a boundary
// (container timescale, manifest, alignment statistics) must go through this
// package so that results are reproducible on arm64 and amd64.
package clock

import (
	"errors"
	"fmt"
	"math/big"
)

// Rational is a non-negative time base expressed as Num/Den. Values are stored
// in lowest terms with a positive denominator.
type Rational struct {
	Num int64
	Den int64
}

// ErrInvalidRational is returned for degenerate time bases.
var ErrInvalidRational = errors.New("clock: invalid rational")

// New returns a normalised rational. It rejects a zero or negative
// denominator and negatives that would flip the sign.
func New(num, den int64) (Rational, error) {
	if den == 0 {
		return Rational{}, ErrInvalidRational
	}
	if num < 0 || den < 0 {
		return Rational{}, ErrInvalidRational
	}
	return normalize(num, den), nil
}

// Must is New for compile-time constants in fixtures and tests.
func Must(num, den int64) Rational {
	r, err := New(num, den)
	if err != nil {
		panic(err)
	}
	return r
}

func normalize(num, den int64) Rational {
	if num == 0 {
		return Rational{Num: 0, Den: 1}
	}
	a, b := num, den
	for b != 0 {
		a, b = b, a%b
	}
	return Rational{Num: num / a, Den: den / a}
}

// IsZero reports whether the rational is zero.
func (r Rational) IsZero() bool { return r.Num == 0 }

// Valid reports whether the rational has a usable denominator.
func (r Rational) Valid() bool { return r.Den > 0 }

// Float returns the nearest float64. It is used only for reporting, never for
// deriving timestamps.
func (r Rational) Float() float64 { return float64(r.Num) / float64(r.Den) }

// MulInt multiplies the rational by an integer, keeping the result normalised.
func (r Rational) MulInt(n int64) Rational {
	if n == 0 || r.Num == 0 {
		return Rational{Den: 1}
	}
	if n < 0 {
		return Rational{Num: -r.MulInt(-n).Num, Den: r.MulInt(-n).Den}
	}
	// Reduce the multiplier against the denominator, never against the
	// numerator: n is a numerator factor and cancelling it with Den keeps the
	// value exact while limiting intermediate size.
	den := r.Den
	g := gcd(n, den)
	n /= g
	den /= g
	return normalize(r.Num*n, den)
}

func gcd(a, b int64) int64 {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		return 1
	}
	return a
}

// Inv returns the reciprocal.
func (r Rational) Inv() (Rational, error) {
	if r.Num == 0 {
		return Rational{}, ErrInvalidRational
	}
	return New(r.Den, r.Num)
}

// Add returns r + other.
func (r Rational) Add(other Rational) Rational {
	// Keep intermediates exact by using big ints when the products overflow.
	if r.Num == 0 {
		return other
	}
	if other.Num == 0 {
		return r
	}
	bg := new(big.Int).Mul(big.NewInt(r.Num), big.NewInt(other.Den))
	bg.Add(bg, new(big.Int).Mul(big.NewInt(other.Num), big.NewInt(r.Den)))
	bd := new(big.Int).Mul(big.NewInt(r.Den), big.NewInt(other.Den))
	// Reduce before converting back.
	g := new(big.Int).GCD(nil, nil, bg, bd)
	bg.Quo(bg, g)
	bd.Quo(bd, g)
	if !bg.IsInt64() || !bd.IsInt64() {
		return Rational{Num: 0, Den: 1}
	}
	return Rational{Num: bg.Int64(), Den: bd.Int64()}
}

// Cmp compares r and other.
func (r Rational) Cmp(other Rational) int {
	l := new(big.Int).Mul(big.NewInt(r.Num), big.NewInt(other.Den))
	rr := new(big.Int).Mul(big.NewInt(other.Num), big.NewInt(r.Den))
	return l.Cmp(rr)
}

// Rounding selects the tie-breaking rule for rescaling.
type Rounding int

const (
	// RoundFloor truncates towards negative infinity.
	RoundFloor Rounding = iota
	// RoundHalfUp rounds .5 away from zero. It is the project default for
	// manifest timestamps because it is stable across platforms.
	RoundHalfUp
)

func (m Rounding) String() string {
	switch m {
	case RoundFloor:
		return "floor"
	case RoundHalfUp:
		return "half-up"
	default:
		return "unknown"
	}
}

// Rescale converts v units of the source base into units of the destination
// base using the requested rounding rule. Both bases must be valid.
func Rescale(v int64, from, to Rational) (int64, error) {
	return RescaleRound(v, from, to, RoundHalfUp)
}

// RescaleRound is Rescale with an explicit rounding rule.
func RescaleRound(v int64, from, to Rational, mode Rounding) (int64, error) {
	if !from.Valid() || !to.Valid() || from.IsZero() || to.IsZero() {
		return 0, ErrInvalidRational
	}
	// v * from / to == v * (from.Num*to.Den) / (from.Den*to.Num)
	num := new(big.Int).Mul(big.NewInt(v), big.NewInt(from.Num))
	num.Mul(num, big.NewInt(to.Den))
	den := new(big.Int).Mul(big.NewInt(from.Den), big.NewInt(to.Num))
	if den.Sign() == 0 {
		return 0, ErrInvalidRational
	}
	switch mode {
	case RoundFloor:
		q := new(big.Int)
		m := new(big.Int)
		q.QuoRem(num, den, m)
		if m.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		}
		return q.Int64(), nil
	case RoundHalfUp:
		// q = (num + den/2) / den for positive den.
		adj := new(big.Int).Mul(big.NewInt(2), num)
		adj.Add(adj, den)
		den2 := new(big.Int).Mul(big.NewInt(2), den)
		q := new(big.Int).Quo(adj, den2)
		return q.Int64(), nil
	default:
		return 0, fmt.Errorf("clock: unknown rounding mode %d", mode)
	}
}

// Ticks converts a duration expressed in seconds (num/den) into ticks for the
// given timescale using half-up rounding.
func Ticks(secondsNum, secondsDen int64, timescale uint32) (int64, error) {
	if timescale == 0 || secondsDen <= 0 {
		return 0, ErrInvalidRational
	}
	return RescaleRound(secondsNum, Must(1, secondsDen), Must(1, int64(timescale)), RoundHalfUp)
}

// FromTicks converts a tick count at timescale into a rational number of
// seconds.
func FromTicks(ticks int64, timescale uint32) (Rational, error) {
	if timescale == 0 {
		return Rational{}, ErrInvalidRational
	}
	return New(ticks, int64(timescale))
}

// Millis converts a rational duration to milliseconds with half-up rounding.
func (r Rational) Millis() int64 {
	ms, err := RescaleRound(1, r, Must(1, 1000), RoundHalfUp)
	if err != nil {
		return 0
	}
	return ms
}

// MillisF converts to milliseconds as float64 for reporting only.
func (r Rational) MillisF() float64 {
	v := new(big.Rat).SetFrac(big.NewInt(r.Num), big.NewInt(r.Den))
	v.Mul(v, big.NewRat(1000, 1))
	f, _ := v.Float64()
	return f
}

// FrameTime returns the presentation time of frame index at the given frame
// rate, expressed in seconds, exactly.
func FrameTime(index int64, fpsNum, fpsDen int64) Rational {
	if fpsNum <= 0 || fpsDen <= 0 {
		return Rational{Den: 1}
	}
	return normalize(index*fpsDen, fpsNum)
}

// SampleTime returns the presentation time of audio sample index at the given
// sample rate, expressed in seconds, exactly.
func SampleTime(index int64, sampleRate int64) Rational {
	if sampleRate <= 0 {
		return Rational{Den: 1}
	}
	return normalize(index, sampleRate)
}

// AbsDiffMs returns |a-b| in milliseconds as a float64 for statistics.
func AbsDiffMs(a, b Rational) float64 {
	d := a.Add(Rational{Num: -b.Num, Den: b.Den})
	if d.Num < 0 {
		d.Num = -d.Num
	}
	return d.MillisF()
}
