// Package rat holds overflow-checked integer arithmetic shared by the clock
// model. Keeping it separate lets the time base reuse a single, well-tested
// multiply-divide without importing math/bits everywhere.
package rat

import "math/bits"

// ErrOverflow is returned when an operation cannot be represented in 64 bits.
var ErrOverflow = errOverflow{}

type errOverflow struct{}

func (errOverflow) Error() string { return "integer overflow" }

// GCD returns the greatest common divisor of a and b, always non-negative.
func GCD(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	return a
}

// MulDiv computes a*b/c with a 128-bit intermediate. It returns ErrOverflow
// if either the product or the quotient leaves the 64-bit range.
func MulDiv(a, b, c uint64) (uint64, error) {
	if c == 0 {
		return 0, ErrOverflow
	}
	hi, lo := bits.Mul64(a, b)
	if hi >= c {
		return 0, ErrOverflow
	}
	q, _ := bits.Div64(hi, lo, c)
	if q > 1<<63 {
		return 0, ErrOverflow
	}
	return q, nil
}
