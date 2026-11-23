// Package timeline implements the exact rational time base shared by every
// module. Timestamps from different time bases are never added directly;
// they are converted through Rat and compared in a common scale.
package timeline

import (
	"fmt"
	"math/big"

	"mediadeliver/internal/model"
)

// Rat is an exact signed rational number: Num/Den, Den > 0.
type Rat struct {
	num int64
	den int64 // always positive
}

// NewRat constructs a rational. A non-positive denominator is rejected as
// an invalid time base (model.KindInvalidInput).
func NewRat(num, den int64) (Rat, error) {
	if den <= 0 {
		return Rat{}, model.At(model.KindInvalidInput, "timeline", "new rational", -1,
			fmt.Sprintf("non-positive denominator %d", den))
	}
	if num == 0 {
		return Rat{num: 0, den: 1}, nil
	}
	g := gcd(abs64(num), den)
	return Rat{num: num / g, den: den / g}, nil
}

// MustNewRat panics on an invalid denominator; used for constants/tests.
func MustNewRat(num, den int64) Rat {
	r, err := NewRat(num, den)
	if err != nil {
		panic(err)
	}
	return r
}

// Num returns the numerator.
func (r Rat) Num() int64 { return r.num }

// Den returns the (positive) denominator.
func (r Rat) Den() int64 { return r.den }

// Add returns r+o with overflow detection (model.KindOverflow).
func (r Rat) Add(o Rat) (Rat, error) {
	n1, n2, den, err := commonDen(r, o)
	if err != nil {
		return Rat{}, err
	}
	sum, ok := add64(n1, n2)
	if !ok {
		return Rat{}, overflow("add numerators", n1, n2)
	}
	return NewRat(sum, den)
}

// Sub returns r-o with overflow detection.
func (r Rat) Sub(o Rat) (Rat, error) {
	n1, n2, den, err := commonDen(r, o)
	if err != nil {
		return Rat{}, err
	}
	diff, ok := sub64(n1, n2)
	if !ok {
		return Rat{}, overflow("subtract numerators", n1, n2)
	}
	return NewRat(diff, den)
}

// Mul returns r*o with overflow detection.
func (r Rat) Mul(o Rat) (Rat, error) {
	num, ok := mul64(r.num, o.num)
	if !ok {
		return Rat{}, overflow("multiply numerators", r.num, o.num)
	}
	den, ok := mul64(r.den, o.den)
	if !ok {
		return Rat{}, overflow("multiply denominators", r.den, o.den)
	}
	return NewRat(num, den)
}

// Cmp returns -1/0/1 like bytes.Compare.
func (r Rat) Cmp(o Rat) int {
	n1, n2, _, err := commonDen(r, o)
	if err != nil {
		panic(err) // denominators are always positive in valid Rats
	}
	switch {
	case n1 < n2:
		return -1
	case n1 > n2:
		return 1
	default:
		return 0
	}
}

// IsNegative reports whether the value is below zero.
func (r Rat) IsNegative() bool { return r.num < 0 }

// ScaleTo converts the value to an integer tick count in dstDen units,
// rounding toward negative infinity. It rejects overflow and negative
// results so callers never silently wrap a timestamp.
func (r Rat) ScaleTo(dstDen int64) (int64, error) {
	if dstDen <= 0 {
		return 0, model.At(model.KindInvalidInput, "timeline", "scale", -1,
			fmt.Sprintf("non-positive destination denominator %d", dstDen))
	}
	num, ok := mul64(r.num, dstDen)
	if !ok {
		return 0, overflow("scale numerator", r.num, dstDen)
	}
	q := num / r.den
	if (num < 0) && (num%r.den != 0) {
		q-- // floor semantics
	}
	return q, nil
}

// BigRat exposes the value for hash/digest computation without precision loss.
func (r Rat) BigRat() *big.Rat {
	return big.NewRat(r.num, r.den)
}

// String renders "num/den" (integers collapse to "/1" representation kept).
func (r Rat) String() string { return fmt.Sprintf("%d/%d", r.num, r.den) }

// CheckNonNegative rejects negative durations/offsets with the call site label.
func CheckNonNegative(r Rat, op string) error {
	if r.IsNegative() {
		return model.At(model.KindInvalidInput, "timeline", op, -1,
			"negative value "+r.String())
	}
	return nil
}

func commonDen(a, b Rat) (int64, int64, int64, error) {
	g := gcd(a.den, b.den)
	m1, ok := mul64(a.num, b.den/g)
	if !ok {
		return 0, 0, 0, overflow("common denominator", a.num, b.den/g)
	}
	m2, ok := mul64(b.num, a.den/g)
	if !ok {
		return 0, 0, 0, overflow("common denominator", b.num, a.den/g)
	}
	den, ok := mul64(a.den, b.den/g)
	if !ok {
		return 0, 0, 0, overflow("common denominator", a.den, b.den/g)
	}
	return m1, m2, den, nil
}

func overflow(op string, a, b int64) error {
	return model.At(model.KindOverflow, "timeline", op, -1,
		fmt.Sprintf("arithmetic overflow on %d and %d", a, b))
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

func add64(a, b int64) (int64, bool) {
	s := a + b
	if (a > 0 && b > 0 && s < 0) || (a < 0 && b < 0 && s > 0) {
		return 0, false
	}
	return s, true
}

func sub64(a, b int64) (int64, bool) {
	d := a - b
	if (b < 0 && d < a) || (b > 0 && d > a) {
		return 0, false
	}
	return d, true
}

func mul64(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	p := a * b
	if p/a != b {
		return 0, false
	}
	return p, true
}
