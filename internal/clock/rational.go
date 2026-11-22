// Package clock provides the single exact time base shared by every
// archivecheck subsystem.  All media times are represented as rational
// seconds so that Matroska tick counts, per-track scales and millisecond
// manifest tolerances can be compared without floating point drift.
package clock

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// Rational is an immutable exact rational number.  The zero value is 0.
type Rational struct {
	rat *big.Rat
}

// New returns num/den.  The denominator must be positive.
func New(num, den int64) (Rational, error) {
	if den <= 0 {
		return Rational{}, fmt.Errorf("clock: denominator must be positive, got %d", den)
	}
	return Rational{rat: new(big.Rat).SetFrac64(num, den)}, nil
}

// FromTicks converts a media tick count using the given timescale
// (ticks per second) into exact seconds.
func FromTicks(ticks, timescale int64) (Rational, error) {
	if timescale <= 0 {
		return Rational{}, fmt.Errorf("clock: timescale must be positive, got %d", timescale)
	}
	return Rational{rat: new(big.Rat).SetFrac64(ticks, timescale)}, nil
}

// ParseDecimal parses a decimal or "a/b" string exactly.  It is used for
// Matroska TrackTimestampScale values (for example "1.001").
func ParseDecimal(s string) (Rational, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Rational{}, fmt.Errorf("clock: empty decimal")
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return Rational{}, fmt.Errorf("clock: invalid rational %q", s)
	}
	return Rational{rat: r}, nil
}

// MustParseDecimal is ParseDecimal for package-level constants and tests.
func MustParseDecimal(s string) Rational {
	r, err := ParseDecimal(s)
	if err != nil {
		panic(err)
	}
	return r
}

// FromTicksMust is FromTicks for values already known to be valid.
func FromTicksMust(ticks, timescale int64) Rational {
	r, err := FromTicks(ticks, timescale)
	if err != nil {
		panic(err)
	}
	return r
}

// MustNew is New for package-level constants and tests.
func MustNew(num, den int64) Rational {
	r, err := New(num, den)
	if err != nil {
		panic(err)
	}
	return r
}

func (r Rational) v() *big.Rat {
	if r.rat == nil {
		return new(big.Rat)
	}
	return r.rat
}

// Add returns r+o.
func (r Rational) Add(o Rational) Rational {
	return Rational{rat: new(big.Rat).Add(r.v(), o.v())}
}

// Sub returns r-o.
func (r Rational) Sub(o Rational) Rational {
	return Rational{rat: new(big.Rat).Sub(r.v(), o.v())}
}

// Mul returns r*o.
func (r Rational) Mul(o Rational) Rational {
	return Rational{rat: new(big.Rat).Mul(r.v(), o.v())}
}

// Quo returns r/o.  A zero divisor yields a zero value.
func (r Rational) Quo(o Rational) Rational {
	if o.v().Sign() == 0 {
		return Rational{}
	}
	return Rational{rat: new(big.Rat).Quo(r.v(), o.v())}
}

// Neg returns -r.
func (r Rational) Neg() Rational {
	return Rational{rat: new(big.Rat).Neg(r.v())}
}

// Abs returns |r|.
func (r Rational) Abs() Rational {
	return Rational{rat: new(big.Rat).Abs(r.v())}
}

// Cmp returns -1, 0 or 1 comparing r with o.
func (r Rational) Cmp(o Rational) int { return r.v().Cmp(o.v()) }

// Equal reports exact equality.
func (r Rational) Equal(o Rational) bool { return r.Cmp(o) == 0 }

// Sign returns -1, 0 or 1.
func (r Rational) Sign() int { return r.v().Sign() }

// IsZero reports whether r is exactly zero.
func (r Rational) IsZero() bool { return r.v().Sign() == 0 }

// IsNegative reports whether r is below zero.
func (r Rational) IsNegative() bool { return r.v().Sign() < 0 }

// Float64 returns the nearest float64 representation.  It is only for
// reporting; all decisions use exact comparisons.
func (r Rational) Float64() float64 { f, _ := r.v().Float64(); return f }

// Millis returns r expressed in milliseconds.
func (r Rational) Millis() float64 { return r.Float64() * 1000 }

// String renders a terminating decimal exactly, otherwise "num/den".
func (r Rational) String() string {
	rat := r.v()
	if rat.IsInt() {
		return rat.Num().String()
	}
	// Terminating decimal iff denominator has only 2 and 5 as factors.
	den := new(big.Int).Set(rat.Denom())
	two, five := big.NewInt(2), big.NewInt(5)
	digits := 0
	for new(big.Int).Mod(den, two).Sign() == 0 {
		den.Div(den, two)
		digits++
	}
	fives := 0
	for new(big.Int).Mod(den, five).Sign() == 0 {
		den.Div(den, five)
		fives++
	}
	if den.Cmp(big.NewInt(1)) != 0 {
		return rat.RatString()
	}
	if fives > digits {
		digits = fives
	}
	return strings.TrimRight(strings.TrimRight(rat.FloatString(digits), "0"), ".")
}

// MarshalJSON encodes the exact rational as "num/den".
func (r Rational) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.v().RatString())
}

// UnmarshalJSON decodes "num/den" or a decimal string.
func (r *Rational) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	p, err := ParseDecimal(s)
	if err != nil {
		return err
	}
	*r = p
	return nil
}

// Min returns the smaller of a and b.
func Min(a, b Rational) Rational {
	if a.Cmp(b) <= 0 {
		return a
	}
	return b
}

// Max returns the larger of a and b.
func Max(a, b Rational) Rational {
	if a.Cmp(b) >= 0 {
		return a
	}
	return b
}

// WithinTolerance reports |a-b| <= tol using exact arithmetic.
func WithinTolerance(a, b, tol Rational) bool {
	return a.Sub(b).Abs().Cmp(tol) <= 0
}
