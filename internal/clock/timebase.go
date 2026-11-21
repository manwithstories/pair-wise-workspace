// Package clock provides exact rational media time arithmetic. All media
// timestamps are kept as integers in a declared timescale and are only
// converted to seconds as reduced rational numbers, never as floats.
package clock

import (
	"errors"
	"fmt"
	"math/big"
)

// Rational is a reduced fraction. The denominator is always positive.
type Rational struct {
	Num int64
	Den int64
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
	return a
}

// NewRational reduces num/den. A zero denominator is rejected.
func NewRational(num, den int64) (Rational, error) {
	if den == 0 {
		return Rational{}, errors.New("clock: zero denominator")
	}
	if den < 0 {
		num, den = -num, -den
	}
	g := gcd(num, den)
	if g > 1 {
		num /= g
		den /= g
	}
	return Rational{Num: num, Den: den}, nil
}

// MustRational is NewRational for constants known to be valid.
func MustRational(num, den int64) Rational {
	r, err := NewRational(num, den)
	if err != nil {
		panic(err)
	}
	return r
}

func (r Rational) Float() float64 { return float64(r.Num) / float64(r.Den) }

func (r Rational) String() string {
	if r.Den == 1 {
		return fmt.Sprintf("%d", r.Num)
	}
	return fmt.Sprintf("%d/%d", r.Num, r.Den)
}

func (r Rational) IsZero() bool { return r.Num == 0 }

func (r Rational) Cmp(o Rational) int {
	l := big.NewInt(r.Num)
	l.Mul(l, big.NewInt(o.Den))
	rr := big.NewInt(o.Num)
	rr.Mul(rr, big.NewInt(r.Den))
	return l.Cmp(rr)
}

// Add returns r+o reduced. Values are bounded by media durations, so int64
// arithmetic is sufficient after cross-multiplication.
func (r Rational) Add(o Rational) Rational {
	return reduce(r.Num*o.Den+o.Num*r.Den, r.Den*o.Den)
}

func (r Rational) Sub(o Rational) Rational {
	return reduce(r.Num*o.Den-o.Num*r.Den, r.Den*o.Den)
}

func (r Rational) Mul(o Rational) Rational {
	return reduce(r.Num*o.Num, r.Den*o.Den)
}

func reduce(num, den int64) Rational {
	if den < 0 {
		num, den = -num, -den
	}
	g := gcd(num, den)
	if g > 1 {
		num /= g
		den /= g
	}
	return Rational{Num: num, Den: den}
}

// Span is a half-open media time range expressed in seconds.
type Span struct {
	Start Rational
	End   Rational
}

func (s Span) String() string { return s.Start.String() + ".." + s.End.String() }

// Duration returns End-Start.
func (s Span) Duration() Rational { return s.End.Sub(s.Start) }

// Timebase is a positive rational number of ticks per second.
type Timebase struct {
	Rate Rational
}

// New validates and reduces a timescale such as 30000/1001.
func New(num, den int64) (Timebase, error) {
	if num <= 0 || den <= 0 {
		return Timebase{}, fmt.Errorf("clock: invalid timebase %d/%d", num, den)
	}
	r, err := NewRational(num, den)
	if err != nil {
		return Timebase{}, err
	}
	return Timebase{Rate: r}, nil
}

func Must(num, den int64) Timebase {
	tb, err := New(num, den)
	if err != nil {
		panic(err)
	}
	return tb
}

func (tb Timebase) String() string { return tb.Rate.String() + " Hz" }

// Duration converts a tick count to reduced seconds.
func (tb Timebase) Duration(ticks int64) Rational {
	return reduce(ticks*tb.Rate.Den, tb.Rate.Num)
}

// Span converts a start tick and duration to a seconds range.
func (tb Timebase) Span(start, duration int64) Span {
	a := tb.Duration(start)
	return Span{Start: a, End: a.Add(tb.Duration(duration))}
}

// DivisibleError reports an inexact tick conversion. It deliberately carries
// both operands so callers can attach track and timebase context.
type DivisibleError struct {
	Ticks int64
	From  Timebase
	To    Timebase
	Num   int64
	Den   int64
}

func (e *DivisibleError) Error() string {
	return fmt.Sprintf("clock: %d ticks at %s do not divide exactly into %s (%d/%d)",
		e.Ticks, e.From, e.To, e.Num, e.Den)
}

// RescaleExact converts ticks between timebases and fails when the result is
// not an integer. It never silently rounds.
func RescaleExact(ticks int64, from, to Timebase) (int64, error) {
	// seconds = ticks * from.Den/from.Num
	// target = seconds * to.Num/to.Den
	num := big.NewInt(ticks)
	num.Mul(num, big.NewInt(from.Rate.Den))
	num.Mul(num, big.NewInt(to.Rate.Num))
	den := big.NewInt(from.Rate.Num)
	den.Mul(den, big.NewInt(to.Rate.Den))
	quo, rem := new(big.Int), new(big.Int)
	quo.QuoRem(num, den, rem)
	if rem.Sign() != 0 {
		return 0, &DivisibleError{Ticks: ticks, From: from, To: to, Num: num.Int64(), Den: den.Int64()}
	}
	if !quo.IsInt64() {
		return 0, fmt.Errorf("clock: rescaled tick %s overflows int64", quo)
	}
	return quo.Int64(), nil
}

// RescaleNearest converts ticks with round-half-away-from-zero. Use only for
// display or scheduling; storage decisions must use RescaleExact.
func RescaleNearest(ticks int64, from, to Timebase) int64 {
	num := big.NewInt(ticks)
	num.Mul(num, big.NewInt(from.Rate.Den))
	num.Mul(num, big.NewInt(to.Rate.Num))
	den := big.NewInt(from.Rate.Num)
	den.Mul(den, big.NewInt(to.Rate.Den))
	quo, rem := new(big.Int), new(big.Int)
	quo.QuoRem(num, den, rem)
	if rem.Sign() == 0 {
		return quo.Int64()
	}
	twice := new(big.Int).Lsh(new(big.Int).Abs(rem), 1)
	if twice.Cmp(new(big.Int).Abs(den)) >= 0 {
		if num.Sign() < 0 {
			quo.Sub(quo, big.NewInt(1))
		} else {
			quo.Add(quo, big.NewInt(1))
		}
	}
	return quo.Int64()
}

// OrderError marks the first index whose timestamp does not increase.
type OrderError struct {
	Index int
	Prev  int64
	Cur   int64
}

func (e *OrderError) Error() string {
	return fmt.Sprintf("clock: timestamp %d at index %d is not after %d", e.Cur, e.Index, e.Prev)
}

// CheckMonotonic verifies strictly increasing ticks. Equal timestamps are
// rejected because they make seek ordering ambiguous.
func CheckMonotonic(ticks []int64) error {
	for i := 1; i < len(ticks); i++ {
		if ticks[i] <= ticks[i-1] {
			return &OrderError{Index: i, Prev: ticks[i-1], Cur: ticks[i]}
		}
	}
	return nil
}
