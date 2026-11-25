// Package clock provides the exact rational time base shared by every
// analyzer, repairer and the pipeline. All modules express media time as
// Ratio seconds so spans from different sources compare exactly.
package clock

import (
	"errors"
	"fmt"
	"math/big"
)

var (
	// ErrOverflow rejects arithmetic that no longer fits int64 num/den.
	ErrOverflow = errors.New("clock: rational overflow")
	// ErrZeroDenominator rejects a zero denominator.
	ErrZeroDenominator = errors.New("clock: zero denominator")
)

// Ratio is an exact rational number of seconds, reduced with Den > 0.
type Ratio struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

// New returns num/den reduced to lowest terms with a positive denominator.
func New(num, den int64) (Ratio, error) {
	if den == 0 {
		return Ratio{}, ErrZeroDenominator
	}
	return fromBig(new(big.Rat).SetFrac64(num, den))
}

// Ticks converts v ticks at the given timescale into seconds.
func Ticks(v, timescale int64) (Ratio, error) {
	if timescale <= 0 {
		return Ratio{}, fmt.Errorf("clock: invalid timescale %d", timescale)
	}
	return New(v, timescale)
}

// Zero is the origin of the shared time base.
func Zero() Ratio { return Ratio{0, 1} }

func fromBig(r *big.Rat) (Ratio, error) {
	if !r.Num().IsInt64() || !r.Denom().IsInt64() {
		return Ratio{}, ErrOverflow
	}
	return Ratio{Num: r.Num().Int64(), Den: r.Denom().Int64()}, nil
}

func (a Ratio) big() *big.Rat { return new(big.Rat).SetFrac64(a.Num, a.Den) }

// Add returns a+b, rejecting overflow.
func (a Ratio) Add(b Ratio) (Ratio, error) { return fromBig(new(big.Rat).Add(a.big(), b.big())) }

// Sub returns a-b, rejecting overflow.
func (a Ratio) Sub(b Ratio) (Ratio, error) { return fromBig(new(big.Rat).Sub(a.big(), b.big())) }

// Mul returns a*b, rejecting overflow.
func (a Ratio) Mul(b Ratio) (Ratio, error) { return fromBig(new(big.Rat).Mul(a.big(), b.big())) }

// Cmp returns -1, 0 or +1 as a is less than, equal to or greater than b.
func (a Ratio) Cmp(b Ratio) int { return a.big().Cmp(b.big()) }

// Sign reports the sign of a.
func (a Ratio) Sign() int { return a.big().Sign() }

// Seconds approximates a as float64 seconds.
func (a Ratio) Seconds() float64 { f, _ := a.big().Float64(); return f }

// Millis approximates a as float64 milliseconds.
func (a Ratio) Millis() float64 { return a.Seconds() * 1000 }

func (a Ratio) String() string {
	if a.Den == 0 {
		return "0"
	}
	return a.big().RatString()
}

// Min returns the smaller of a and b.
func Min(a, b Ratio) Ratio {
	if a.Cmp(b) <= 0 {
		return a
	}
	return b
}

// Max returns the larger of a and b.
func Max(a, b Ratio) Ratio {
	if a.Cmp(b) >= 0 {
		return a
	}
	return b
}
