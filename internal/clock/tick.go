// Package clock defines the rational time base shared by the container
// scanner, the slicing layer and the audio pipeline.
//
// All time math stays in integer rational units. Converting between
// timescales never accumulates floating point drift.
package clock

import (
	"errors"
	"fmt"
)

// Tick is an absolute media timestamp on a rational time base: Value ticks
// equal Value/Timescale seconds. The zero value is not valid; construct
// ticks with New.
type Tick struct {
	Value     uint64
	Timescale uint32
}

// New builds a Tick. A zero timescale has no meaning and is rejected.
func New(value uint64, timescale uint32) (Tick, error) {
	if timescale == 0 {
		return Tick{}, errors.New("clock: zero timescale")
	}
	return Tick{Value: value, Timescale: timescale}, nil
}

// MustNew is New for cases where the timescale is a compile-time constant.
func MustNew(value uint64, timescale uint32) Tick {
	t, err := New(value, timescale)
	if err != nil {
		panic(err)
	}
	return t
}

// Seconds returns the exact rational duration as numerator/denominator.
func (t Tick) Seconds() (num, den uint64) {
	return t.Value, uint64(t.Timescale)
}

// Millis rounds the timestamp to whole milliseconds (nearest, half up).
func (t Tick) Millis() uint64 {
	return (t.Value*1000 + uint64(t.Timescale)/2) / uint64(t.Timescale)
}

// To rescales the tick onto dstTimescale using one integer multiplication
// and one division (round to nearest). There is no float accumulation.
func (t Tick) To(dstTimescale uint32) (Tick, error) {
	if dstTimescale == 0 {
		return Tick{}, errors.New("clock: zero destination timescale")
	}
	if t.Timescale == 0 {
		return Tick{}, errors.New("clock: tick has no timescale")
	}
	v := t.Value*uint64(dstTimescale) + uint64(t.Timescale)/2
	return Tick{Value: v / uint64(t.Timescale), Timescale: dstTimescale}, nil
}

// Cmp compares two ticks after rescaling both to their LCM timescale. It
// returns -1, 0 or 1. Ticks on different timescales compare exactly.
func (t Tick) Cmp(other Tick) (int, error) {
	if t.Timescale == 0 || other.Timescale == 0 {
		return 0, errors.New("clock: tick has no timescale")
	}
	g := gcd(uint64(t.Timescale), uint64(other.Timescale))
	lcm := uint64(t.Timescale) / g * uint64(other.Timescale)
	a, err := t.To(uint32(lcm))
	if err != nil {
		return 0, err
	}
	b, err := other.To(uint32(lcm))
	if err != nil {
		return 0, err
	}
	switch {
	case a.Value < b.Value:
		return -1, nil
	case a.Value > b.Value:
		return 1, nil
	default:
		return 0, nil
	}
}

// Add returns t+d on t's timescale. Both operands must share the timescale.
func (t Tick) Add(d Duration) (Tick, error) {
	if t.Timescale != d.Timescale || t.Timescale == 0 {
		return Tick{}, fmt.Errorf("clock: timescale mismatch %d vs %d", t.Timescale, d.Timescale)
	}
	return Tick{Value: t.Value + d.Ticks, Timescale: t.Timescale}, nil
}

// Sub returns the duration between two ticks sharing one timescale.
func Sub(a, b Tick) (Duration, error) {
	if a.Timescale != b.Timescale || a.Timescale == 0 {
		return Duration{}, fmt.Errorf("clock: timescale mismatch %d vs %d", a.Timescale, b.Timescale)
	}
	if b.Value > a.Value {
		return Duration{}, errors.New("clock: negative duration")
	}
	return Duration{Ticks: a.Value - b.Value, Timescale: a.Timescale}, nil
}

// Duration is a rational length of time on a known timescale.
type Duration struct {
	Ticks     uint64
	Timescale uint32
}

// NewDuration builds a Duration and rejects a zero timescale.
func NewDuration(ticks uint64, timescale uint32) (Duration, error) {
	if timescale == 0 {
		return Duration{}, errors.New("clock: zero timescale")
	}
	return Duration{Ticks: ticks, Timescale: timescale}, nil
}

// Millis rounds the duration to whole milliseconds (nearest, half up).
func (d Duration) Millis() uint64 {
	return (d.Ticks*1000 + uint64(d.Timescale)/2) / uint64(d.Timescale)
}

func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
