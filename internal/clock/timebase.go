// Package clock provides exact rational time-base conversion for MP4 tracks.
// All conversions use integers or math/big; floating point is never used for
// accumulating or mapping timestamps.
package clock

import (
	"errors"
	"fmt"
	"math"
	"math/big"
)

// ErrZeroTimescale is returned when a time base is built from zero.
var ErrZeroTimescale = errors.New("timescale must be greater than zero")

// Timebase is a track clock expressed in ticks per second.
type Timebase struct {
	TicksPerSecond uint32
}

// New validates and returns a Timebase for the supplied timescale.
func New(timescale uint32) (Timebase, error) {
	if timescale == 0 {
		return Timebase{}, ErrZeroTimescale
	}
	return Timebase{TicksPerSecond: timescale}, nil
}

// MustNew is New for package-internal constants and tests. It panics on a
// zero timescale so callers cannot silently lose time precision.
func MustNew(timescale uint32) Timebase {
	tb, err := New(timescale)
	if err != nil {
		panic(err)
	}
	return tb
}

// TickDuration returns the exact duration of one tick in seconds.
func (t Timebase) TickDuration() *big.Rat {
	if t.TicksPerSecond == 0 {
		return new(big.Rat)
	}
	return new(big.Rat).SetFrac64(1, int64(t.TicksPerSecond))
}

// Seconds returns the exact duration represented by ticks in this time base.
func (t Timebase) Seconds(ticks uint64) *big.Rat {
	r := t.TickDuration()
	r.Mul(r, new(big.Rat).SetUint64(ticks))
	return r
}

// RescaleRat maps ticks from this time base into target and returns the exact
// rational result. The caller decides how to round.
func (t Timebase) RescaleRat(ticks uint64, target Timebase) (*big.Rat, error) {
	if t.TicksPerSecond == 0 || target.TicksPerSecond == 0 {
		return nil, ErrZeroTimescale
	}
	num := new(big.Int).SetUint64(ticks)
	num.Mul(num, new(big.Int).SetUint64(uint64(target.TicksPerSecond)))
	den := new(big.Int).SetUint64(uint64(t.TicksPerSecond))
	return new(big.Rat).SetFrac(num, den), nil
}

// Rescale maps ticks into target, truncating fractional ticks. It returns an
// error when the result cannot fit in a uint64.
func (t Timebase) Rescale(ticks uint64, target Timebase) (uint64, error) {
	r, err := t.RescaleRat(ticks, target)
	if err != nil {
		return 0, err
	}
	q := new(big.Int).Quo(r.Num(), r.Denom())
	if !q.IsUint64() {
		return 0, fmt.Errorf("rescaled timestamp overflows uint64: ticks=%d from=%d to=%d",
			ticks, t.TicksPerSecond, target.TicksPerSecond)
	}
	return q.Uint64(), nil
}

// Compare returns -1, 0 or 1 comparing tick values in this time base.
func (t Timebase) Compare(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// Mapping describes a cross-track timestamp mapping.
type Mapping struct {
	Source Timebase
	Target Timebase
}

// NewMapping builds a cross-track mapping.
func NewMapping(source, target Timebase) (Mapping, error) {
	if source.TicksPerSecond == 0 || target.TicksPerSecond == 0 {
		return Mapping{}, ErrZeroTimescale
	}
	return Mapping{Source: source, Target: target}, nil
}

// Map converts a source tick value into the target time base, returning the
// truncated tick count and the exact rational value.
func (m Mapping) Map(ticks uint64) (uint64, *big.Rat, error) {
	r, err := m.Source.RescaleRat(ticks, m.Target)
	if err != nil {
		return 0, nil, err
	}
	q := new(big.Int).Quo(r.Num(), r.Denom())
	if !q.IsUint64() {
		return 0, nil, fmt.Errorf("mapped timestamp overflows uint64")
	}
	return q.Uint64(), r, nil
}

// MaxTicks is the largest representable tick count.
const MaxTicks = uint64(math.MaxUint64)
