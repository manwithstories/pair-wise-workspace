// Package core contains the shared rational timebase and error model used by
// scanning, indexing, clock correction, repair, and verification.
package core

import (
	"fmt"
	"math/big"
)

// Rational is an exact signed rational number. The zero value is treated as
// 0/1 by the arithmetic helpers.
type Rational struct {
	N int64 `json:"n"`
	D int64 `json:"d"`
}

// Rat returns a normalized rational. A zero denominator is rejected.
func Rat(n, d int64) (Rational, error) {
	if d == 0 {
		return Rational{}, fmt.Errorf("rational denominator is zero")
	}
	if d < 0 {
		n, d = -n, -d
	}
	g := gcd(abs64(n), abs64(d))
	if g > 0 {
		n /= int64(g)
		d /= int64(g)
	}
	return Rational{N: n, D: d}, nil
}

func MustRat(n, d int64) Rational {
	r, err := Rat(n, d)
	if err != nil {
		panic(err)
	}
	return r
}

func (r Rational) denominator() int64 {
	if r.D == 0 {
		return 1
	}
	return r.D
}

// Mul returns r*x as an exact rational.
func (r Rational) Mul(x Rational) (Rational, error) {
	return Rat(r.N*x.N, r.denominator()*x.denominator())
}

// ScaleFloor returns floor(value * r.N / r.D) without accumulating rounded
// block errors. big.Int keeps clock products safe.
func ScaleFloor(value int64, r Rational) int64 {
	d := r.denominator()
	num := new(big.Int).Mul(big.NewInt(value), big.NewInt(r.N))
	den := big.NewInt(d)
	return floorQuo(num, den).Int64()
}

// DivideFloor returns floor(value / r), where r is interpreted as numerator /
// denominator. This is useful for converting device ticks to frame positions.
func DivideFloor(value int64, r Rational) int64 {
	d := r.denominator()
	// value / (N/D) == value*D/N
	num := new(big.Int).Mul(big.NewInt(value), big.NewInt(d))
	den := big.NewInt(r.N)
	if r.N == 0 {
		if value >= 0 {
			return 0
		}
		return -1
	}
	return floorQuo(num, den).Int64()
}

func floorQuo(num, den *big.Int) *big.Int {
	q := new(big.Int).Quo(num, den)
	r := new(big.Int).Rem(num, den)
	// Quo truncates toward zero. For a non-zero remainder and negative
	// numerator/denominator combination, adjust to mathematical floor.
	if r.Sign() != 0 && num.Sign()*den.Sign() < 0 {
		q.Sub(q, big.NewInt(1))
	}
	return q
}

func abs64(v int64) uint64 {
	if v < 0 {
		return uint64(-v)
	}
	return uint64(v)
}

func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// Timebase is the archive's unified scale. All normalized positions are
// signed sample-frame indices at SampleRate.
type Timebase struct {
	SampleRate int `json:"sample_rate"`
}

func NewTimebase(sampleRate int) (Timebase, error) {
	if sampleRate <= 0 {
		return Timebase{}, New(KindClock, "core.NewTimebase", "sample rate must be positive")
	}
	return Timebase{SampleRate: sampleRate}, nil
}

// FrameSeconds returns the exact duration of frame frames in seconds.
func (tb Timebase) FrameSeconds(frames int64) Rational {
	return MustRat(frames, int64(tb.SampleRate))
}

// AbsoluteFrame maps an absolute, unwrapped device tick directly to a frame.
// Computing every point from the same origin prevents per-block rounding drift.
func (tb Timebase) AbsoluteFrame(tick, originTick, originFrame int64, ticksPerFrame Rational) int64 {
	delta := tick - originTick
	return originFrame + DivideFloor(delta, ticksPerFrame)
}

// CheckMonotonic returns a clock error if positions are not non-decreasing.
func CheckMonotonic(values []int64) error {
	for i := 1; i < len(values); i++ {
		if values[i] < values[i-1] {
			return Newf(KindClock, "core.CheckMonotonic",
				"positions not monotonic at %d: %d < %d", i, values[i], values[i-1])
		}
	}
	return nil
}
