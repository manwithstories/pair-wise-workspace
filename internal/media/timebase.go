// Package media contains the shared 90 kHz time base and unified error model
// used by every relay subsystem.
package media

import (
	"fmt"
	"math"
)

// TickHz is the MPEG/HLS presentation clock frequency.
const TickHz int64 = 90_000

// Tick is a monotonic 90 kHz timestamp or duration expressed in integer ticks.
type Tick int64

// Seconds converts ticks to floating-point seconds. Manifest durations are
// only converted at the final HLS boundary.
func (t Tick) Seconds() float64 {
	return float64(t) / float64(TickHz)
}

// TicksFromSeconds rounds seconds to the nearest integer 90 kHz tick.
func TicksFromSeconds(seconds float64) Tick {
	return Tick(math.Round(seconds * float64(TickHz)))
}

// AlignDown returns the largest multiple of grid not greater than t.
func AlignDown(t, grid Tick) Tick {
	if grid <= 0 {
		return t
	}
	if t < 0 {
		return -((-t + grid - 1) / grid) * grid
	}
	return (t / grid) * grid
}

// AlignUp returns the smallest multiple of grid not less than t.
func AlignUp(t, grid Tick) Tick {
	down := AlignDown(t, grid)
	if t == down || grid <= 0 {
		return down
	}
	return down + grid
}

// BoundaryDelta returns the absolute integer distance from t to boundary.
func BoundaryDelta(t, boundary Tick) Tick {
	if d := t - boundary; d >= 0 {
		return d
	} else {
		return -d
	}
}

// WithinTolerance reports whether a shared boundary discrepancy is legal.
func WithinTolerance(delta, maxTicks Tick) bool {
	return delta >= 0 && delta <= maxTicks
}

// DurationRange describes an integer-tick interval.
type DurationRange struct {
	Start Tick `json:"start90k"`
	End   Tick `json:"end90k"`
}

// Duration returns End-Start in 90 kHz ticks.
func (r DurationRange) Duration() Tick {
	return r.End - r.Start
}

// String renders the range in ticks.
func (r DurationRange) String() string {
	return fmt.Sprintf("%d-%d (%d ticks)", r.Start, r.End, r.Duration())
}
