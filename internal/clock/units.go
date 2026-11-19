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
	// Divide by (1 + ppm/1e6): multiply by 1e6/(1e6+ppm).
	us.Mul(us, big.NewInt(1_000_000))
	den := new(big.Int).Add(big.NewInt(1_000_000), big.NewInt(ppm.Num/ppm.Den))
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
