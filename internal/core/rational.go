package core

import (
	"math"
	"math/bits"
)

// Rat is an integer rational number Num/Den with Den > 0. It is used to
// convert between track time bases without floating point.
type Rat struct {
	Num int64
	Den int64
}

// TickRate returns the rational "seconds per tick" for an ISO timescale
// (ticks per second): 1/rate.
func TickRate(timescale uint32) Rat { return Rat{Num: 1, Den: int64(timescale)} }

// ScaleTicks converts a timestamp v measured in fromRate ticks/second into
// toRate ticks/second, rounding to nearest. The boolean is false when the
// multiplication or result would overflow int64 or a rate is invalid.
func ScaleTicks(v int64, fromRate, toRate uint32) (int64, bool) {
	if fromRate == 0 || toRate == 0 {
		return 0, false
	}
	return MulDivRound(v, int64(toRate), int64(fromRate))
}

// ScaleTicksFloor is ScaleTicks with floor rounding (needed for exact boundary
// comparisons where no rounding drift may be introduced).
func ScaleTicksFloor(v int64, fromRate, toRate uint32) (int64, bool) {
	if fromRate == 0 || toRate == 0 {
		return 0, false
	}
	return MulDivFloor(v, int64(toRate), int64(fromRate))
}

// MulDivFloor computes floor(v*num/den) with 128-bit overflow detection.
func MulDivFloor(v, num, den int64) (int64, bool) {
	return mulDiv(v, num, den, false)
}

// MulDivRound computes round-to-nearest v*num/den with overflow detection.
func MulDivRound(v, num, den int64) (int64, bool) {
	return mulDiv(v, num, den, true)
}

// abs64 returns the magnitude of x as uint64 and whether x is negative. The
// MinInt64 case is represented exactly as 1<<63, which fits a uint64.
func abs64(x int64) (uint64, bool) {
	if x >= 0 {
		return uint64(x), false
	}
	if x == math.MinInt64 {
		return uint64(math.MaxInt64) + 1, true
	}
	return uint64(-x), true
}

// mulDiv returns v*num/den using 128-bit product arithmetic. Signs are applied
// to the signed result; round selects nearest instead of floor magnitude.
func mulDiv(v, num, den int64, round bool) (int64, bool) {
	if den == 0 {
		return 0, false
	}
	if num == 0 {
		return 0, true
	}
	av, vNeg := abs64(v)
	an, numNeg := abs64(num)
	ad, denNeg := abs64(den)
	negative := (vNeg != numNeg) != denNeg

	hi, lo := bits.Mul64(av, an)
	if hi >= ad {
		return 0, false // quotient cannot fit a machine word
	}
	q, rem := bits.Div64(hi, lo, ad)
	if round && rem*2 >= ad {
		q++
	}
	if q > uint64(math.MaxInt64) {
		return 0, false
	}
	if negative {
		return -int64(q), true
	}
	return int64(q), true
}

// AddChecked adds d to t and reports false on int64 overflow.
func AddChecked(t, d int64) (int64, bool) {
	if d > 0 && t > math.MaxInt64-d {
		return 0, false
	}
	if d < 0 && t < math.MinInt64-d {
		return 0, false
	}
	return t + d, true
}
