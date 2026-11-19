package clock

import (
	"math/big"

	"archivearchivist/internal/model"
)

// TicksToUS maps x clock ticks to microseconds with drift compensation.
func TicksToUS(x int64, rate uint32, ppm model.Rational) int64 {
	if rate == 0 {
		return 0
	}
	n := new(big.Int).Mul(big.NewInt(x), big.NewInt(1_000_000))
	us := new(big.Int).Quo(n, big.NewInt(int64(rate)))
	if ppm.Num == 0 {
		if !us.IsInt64() {
			return 0
		}
		return us.Int64()
	}
	// A media clock running ppm slow produces that many fewer ticks per
	// second, so its elapsed time is longer than the tick count suggests:
	// corrected = us / (1 - ppm/1e6) = us * 1e6*den/(1e6*den - num).
	// Multiplying by the rational's denominator keeps this exact.
	den := new(big.Int).Mul(big.NewInt(1_000_000), big.NewInt(ppm.Den))
	den.Sub(den, big.NewInt(ppm.Num))
	if den.Sign() <= 0 {
		// A clock at or past 1e6 ppm is meaningless; refuse to scale.
		if !us.IsInt64() {
			return 0
		}
		return us.Int64()
	}
	us.Mul(us, new(big.Int).Mul(big.NewInt(1_000_000), big.NewInt(ppm.Den)))
	us.Quo(us, den)
	if !us.IsInt64() {
		return 0
	}
	return us.Int64()
}

// ToSeconds converts normalized ticks to seconds with drift compensation.
func ToSeconds(norm int64, rate uint32, ppm model.Rational) model.Rational {
	return model.NewRat(TicksToUS(norm, rate, ppm), 1_000_000)
}

func itoa(v int64) string { return big.NewInt(v).String() }

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
