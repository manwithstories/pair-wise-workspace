// Package clock provides the shared 90 kHz time base used by manifest
// alignment and packet processing, plus audio sample-clock conversion.
package clock

import "fmt"

// VideoClockHz is the unified timeline clock (MPEG 90 kHz).
const VideoClockHz = 90000

// TicksPerFrame30 is the duration of one 30 fps frame in 90 kHz ticks.
const TicksPerFrame30 = VideoClockHz / 30

// Common audio sample rates handled by the pipeline.
const (
	Rate44100 = 44100
	Rate48000 = 48000
)

// Error is a structured timeline error carrying the segment sequence number
// and the interval on the unified 90 kHz time base.
type Error struct {
	Op        string
	Segment   int
	StartTick uint64
	EndTick   uint64
	Msg       string
}

func (e *Error) Error() string {
	return fmt.Sprintf("clock: %s: segment=%d interval=[%d,%d) ticks (%.3f..%.3f ms): %s",
		e.Op, e.Segment, e.StartTick, e.EndTick,
		TicksToMillisFloat(e.StartTick), TicksToMillisFloat(e.EndTick), e.Msg)
}

// TicksToMillis converts 90 kHz ticks to whole milliseconds.
func TicksToMillis(t uint64) uint64 { return t * 1000 / VideoClockHz }

// TicksToMillisFloat converts 90 kHz ticks to fractional milliseconds.
func TicksToMillisFloat(t uint64) float64 { return float64(t) * 1000.0 / VideoClockHz }

// MillisToTicks converts milliseconds to 90 kHz ticks (rounded).
func MillisToTicks(ms float64) uint64 {
	if ms <= 0 {
		return 0
	}
	return uint64(ms*VideoClockHz/1000.0 + 0.5)
}

// SamplesToTicks converts an audio sample count at rate Hz to 90 kHz ticks.
func SamplesToTicks(samples uint64, rate int) uint64 {
	if rate <= 0 {
		return 0
	}
	return samples * VideoClockHz / uint64(rate)
}

// TicksToSamples converts 90 kHz ticks to a sample count at rate Hz.
func TicksToSamples(t uint64, rate int) uint64 {
	if rate <= 0 {
		return 0
	}
	return t * uint64(rate) / VideoClockHz
}
