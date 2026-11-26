// Package media provides the shared rational timebase, source-position error
// model and track mapping used by every other subsystem in aircheck.
package media

import (
	"errors"
	"fmt"
	"math"
)

// Clock is a rational timebase. Timestamps on the wire are integer ticks of
// TicksPerSecond, so every duration in the pipeline is exact: sampling rates
// with different prime factors (e.g. 90 kHz video and 48 kHz audio) can be
// compared without floating point drift.
//
// A Clock is immutable and safe to share between goroutines.
type Clock struct {
	ticksPerSecond int64
}

// ErrInvalidClock is returned for a timebase that cannot index whole seconds.
var ErrInvalidClock = errors.New("media: ticks-per-second must be positive")

// NewClock returns a Clock for the given tick resolution.
func NewClock(ticksPerSecond int64) (Clock, error) {
	if ticksPerSecond <= 0 || ticksPerSecond > 1<<32 {
		return Clock{}, fmt.Errorf("%w: got %d", ErrInvalidClock, ticksPerSecond)
	}
	return Clock{ticksPerSecond: ticksPerSecond}, nil
}

// MustClock is NewClock for compile-time style package initialisation.
func MustClock(ticksPerSecond int64) Clock {
	c, err := NewClock(ticksPerSecond)
	if err != nil {
		panic(err)
	}
	return c
}

// TicksPerSecond reports the tick resolution.
func (c Clock) TicksPerSecond() int64 { return c.ticksPerSecond }

// Valid reports whether the clock was properly constructed.
func (c Clock) Valid() bool { return c.ticksPerSecond > 0 }

// Duration is an exact span of ticks in a specific clock.
type Duration struct {
	ticks int64
	clock Clock
}

// Ticks reports the raw tick count.
func (d Duration) Ticks() int64 { return d.ticks }

// Clock reports the timebase the ticks are expressed in.
func (d Duration) Clock() Clock { return d.clock }

// FromTicks builds a Duration.
func (c Clock) FromTicks(n int64) Duration { return Duration{ticks: n, clock: c} }

// FromSeconds builds a Duration from whole seconds; a negative span is
// permitted so that callers can express lead-in offsets.
func (c Clock) FromSeconds(sec int64) Duration {
	return Duration{ticks: sec * c.ticksPerSecond, clock: c}
}

// FromMilliseconds builds a Duration from whole milliseconds. It fails when
// the clock cannot represent the span exactly; callers that need approximate
// alignment convert a whole number of ticks after rounding themselves.
func (c Clock) FromMilliseconds(ms int64) (Duration, error) {
	num := ms * c.ticksPerSecond
	if num%1000 != 0 {
		return Duration{}, fmt.Errorf("media: %d ms is not exact at %d ticks/s", ms, c.ticksPerSecond)
	}
	return Duration{ticks: num / 1000, clock: c}, nil
}

// FromRational expresses a span num/den seconds exactly, or reports an error.
func (c Clock) FromRational(num, den int64) (Duration, error) {
	if den == 0 {
		return Duration{}, fmt.Errorf("media: zero denominator")
	}
	if num == 0 {
		return Duration{clock: c}, nil
	}
	t := num * c.ticksPerSecond
	if t%den != 0 {
		return Duration{}, fmt.Errorf("media: %d/%d s is not exact at %d ticks/s", num, den, c.ticksPerSecond)
	}
	return Duration{ticks: t / den, clock: c}, nil
}

// Add returns d+e. The clock must match.
func (d Duration) Add(e Duration) (Duration, error) {
	if d.clock != e.clock {
		return Duration{}, fmt.Errorf("media: clock mismatch %d vs %d", d.clock.ticksPerSecond, e.clock.ticksPerSecond)
	}
	return Duration{ticks: d.ticks + e.ticks, clock: d.clock}, nil
}

// Sub returns d-e.
func (d Duration) Sub(e Duration) (Duration, error) {
	if d.clock != e.clock {
		return Duration{}, fmt.Errorf("media: clock mismatch %d vs %d", d.clock.ticksPerSecond, e.clock.ticksPerSecond)
	}
	return Duration{ticks: d.ticks - e.ticks, clock: d.clock}, nil
}

// Convert re-expresses d in another clock, failing if the conversion is not
// exact. This is what lets a 90 kHz video track and a 48 kHz audio track share
// a single rational reference clock.
func (d Duration) Convert(to Clock) (Duration, error) {
	if d.clock == to {
		return d, nil
	}
	if !d.clock.Valid() || !to.Valid() {
		return Duration{}, ErrInvalidClock
	}
	num := d.ticks * to.ticksPerSecond
	if num%d.clock.ticksPerSecond != 0 {
		return Duration{}, fmt.Errorf("media: %d ticks @%d cannot convert exactly to @%d",
			d.ticks, d.clock.ticksPerSecond, to.ticksPerSecond)
	}
	return Duration{ticks: num / d.clock.ticksPerSecond, clock: to}, nil
}

// Seconds returns the span as a float64. Presentation only; use Ticks and
// Convert for computation.
func (d Duration) Seconds() float64 {
	if !d.clock.Valid() {
		return 0
	}
	return float64(d.ticks) / float64(d.clock.ticksPerSecond)
}

// Milliseconds returns the span in milliseconds as a float64.
func (d Duration) Milliseconds() float64 { return d.Seconds() * 1000 }

// String renders the span as seconds with three decimals.
func (d Duration) String() string { return fmt.Sprintf("%.3fs", d.Seconds()) }

// Abs returns the absolute span.
func (d Duration) Abs() Duration {
	if d.ticks < 0 {
		return Duration{ticks: -d.ticks, clock: d.clock}
	}
	return d
}

// Less reports whether d is shorter than e.
func (d Duration) Less(e Duration) bool { return d.ticks < e.ticks }

// Tick is an absolute instant on a track clock, normalised so that zero is the
// start of the presentation window.
type Tick struct {
	value int64
	clock Clock
}

// Clock reports the timebase.
func (t Tick) Clock() Clock { return t.clock }

// Value reports the raw tick.
func (t Tick) Value() int64 { return t.value }

// At builds an absolute instant.
func (c Clock) At(n int64) Tick { return Tick{value: n, clock: c} }

// Add advances the instant by d.
func (t Tick) Add(d Duration) (Tick, error) {
	if t.clock != d.clock {
		return Tick{}, fmt.Errorf("media: tick/clock mismatch")
	}
	return Tick{value: t.value + d.ticks, clock: t.clock}, nil
}

// Sub returns the exact span from u to t.
func (t Tick) Sub(u Tick) (Duration, error) {
	if t.clock != u.clock {
		return Duration{}, fmt.Errorf("media: tick/clock mismatch")
	}
	return Duration{ticks: t.value - u.value, clock: t.clock}, nil
}

// Convert re-expresses the instant in another clock.
func (t Tick) Convert(to Clock) (Tick, error) {
	d, err := Duration{ticks: t.value, clock: t.clock}.Convert(to)
	if err != nil {
		return Tick{}, err
	}
	return Tick{value: d.ticks, clock: to}, nil
}

// Seconds returns the instant in seconds relative to zero.
func (t Tick) Seconds() float64 {
	if !t.clock.Valid() {
		return 0
	}
	return float64(t.value) / float64(t.clock.ticksPerSecond)
}

// Milliseconds returns the instant in milliseconds relative to zero.
func (t Tick) Milliseconds() float64 { return t.Seconds() * 1000 }

// GCD reduces a tick rate to its smallest equivalent representation, useful
// when comparing timescales from MP4 (which may be e.g. 90000 or 48000).
func GCD(a, b int64) int64 {
	if a == 0 {
		return b
	}
	if b == 0 {
		return a
	}
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		return -a
	}
	return a
}

// LCM returns the least common multiple of a and b, guarding overflow.
func LCM(a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, fmt.Errorf("media: lcm of zero")
	}
	g := GCD(a, b)
	x, y := a/g, b
	if y < 0 {
		y = -y
	}
	if x < 0 {
		x = -x
	}
	if x != 0 && y > math.MaxInt64/x {
		return 0, errors.New("media: lcm overflow")
	}
	return x * y, nil
}
