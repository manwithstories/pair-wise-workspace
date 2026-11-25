// Package model defines the shared media time base and the unified fault
// model used by every analyzer subsystem (WAV, RTP, MP4, clock and index).
package model

import (
	"errors"
	"math/big"
)

// Rational is an exact media time base expressed as Num/Den ticks per second.
// It is kept reduced so two equivalent rates compare equal with ==.
type Rational struct {
	Num int64
	Den int64
}

// NewRational builds a reduced rate and rejects a zero denominator.
func NewRational(num, den int64) (Rational, error) {
	if den == 0 {
		return Rational{}, errors.New("model: zero denominator in time base")
	}
	if den < 0 {
		num, den = -num, -den
	}
	g := gcd(abs64(num), den)
	if g == 0 {
		g = 1
	}
	return Rational{Num: num / g, Den: den / g}, nil
}

// MustRational panics instead of returning an error; intended for constants.
func MustRational(num, den int64) Rational {
	r, err := NewRational(num, den)
	if err != nil {
		panic(err)
	}
	return r
}

// Hz builds the common integer-Hertz rate n/1.
func Hz(n int64) Rational { return Rational{Num: n, Den: 1} }

// Seconds converts an exact frame/tick count at this rate to a (possibly
// fractional) number of seconds, using big.Rat to avoid overflow on long
// recordings.
func (r Rational) Seconds(ticks int64) *big.Rat {
	return new(big.Rat).SetFrac(big.NewInt(ticks*r.Num), big.NewInt(r.Den))
}

// Nanos returns the tick count expressed in nanoseconds, rounded to nearest.
// big.Int keeps the product (ticks * rate * 1e9) from overflowing.
func (r Rational) Nanos(ticks int64) int64 {
	num := big.NewInt(ticks)
	num.Mul(num, big.NewInt(r.Den))
	num.Mul(num, big.NewInt(1_000_000_000))
	den := big.NewInt(r.Num)
	return quoRound(num, den).Int64()
}

// TicksFromNanos is the inverse of Nanos: nanoseconds -> ticks, rounded.
func (r Rational) TicksFromNanos(nanos int64) int64 {
	num := big.NewInt(nanos)
	num.Mul(num, big.NewInt(r.Num))
	den := big.NewInt(r.Den)
	den.Mul(den, big.NewInt(1_000_000_000))
	return quoRound(num, den).Int64()
}

// Rescale converts a tick count from rate r to rate to, rounding half away
// from zero. big.Int ensures the intermediate product cannot overflow.
func (r Rational) Rescale(ticks int64, to Rational) int64 {
	num := big.NewInt(ticks)
	num.Mul(num, big.NewInt(r.Den))
	num.Mul(num, big.NewInt(to.Num))
	den := big.NewInt(r.Num)
	den.Mul(den, big.NewInt(to.Den))
	return quoRound(num, den).Int64()
}

// quoRound returns num/den rounded half away from zero.
func quoRound(num, den *big.Int) *big.Int {
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(num, den, rem)
	twoRem := new(big.Int).Abs(rem)
	twoRem.Lsh(twoRem, 1)
	absDen := new(big.Int).Abs(den)
	if twoRem.Cmp(absDen) >= 0 {
		if num.Sign() == den.Sign() {
			q.Add(q, big.NewInt(1))
		} else {
			q.Sub(q, big.NewInt(1))
		}
	}
	return q
}

// Equal reports whether two rates denote the same frequency.
func (r Rational) Equal(o Rational) bool { return r.Num == o.Num && r.Den == o.Den }

// String renders Num/Den.
func (r Rational) String() string {
	if r.Den == 1 {
		return itoa(r.Num)
	}
	return itoa(r.Num) + "/" + itoa(r.Den)
}

// Frame is an absolute, unwrapped sample-frame ordinal on a track.
type Frame int64

// FrameRange is a half-open interval [First, Last) of sample frames.
type FrameRange struct {
	First Frame
	Last  Frame
}

// Len is the number of frames in the range.
func (fr FrameRange) Len() int64 { return int64(fr.Last - fr.First) }

// Contains reports whether f falls inside the half-open interval.
func (fr FrameRange) Contains(f Frame) bool { return f >= fr.First && f < fr.Last }

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

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	var b [20]byte
	i := len(b)
	for v != 0 {
		i--
		c := byte('0' + abs64(v%10))
		b[i] = c
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
