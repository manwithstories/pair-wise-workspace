// Package clock defines the unified media timeline used by every processing
// stage. All internal timing is expressed in integer ticks at TicksPerSecond,
// which keeps reordering, watermark and replay logic deterministic and free of
// floating point drift.
package clock

import "fmt"

// TicksPerSecond is the resolution of the shared media timeline.
const TicksPerSecond int64 = 48_000

// VideoClockRate is the RTP clock rate assumed for video streams.
const VideoClockRate uint32 = 90_000

// AudioClockRate is the RTP clock rate assumed for PCM audio streams.
const AudioClockRate uint32 = 48_000

// Ticks is one point on the unified media timeline.
type Ticks int64

// FromSeconds converts a duration given in seconds to ticks.
func FromSeconds(sec float64) Ticks {
	return Ticks(sec*float64(TicksPerSecond) + 0.5)
}

// Milliseconds converts a millisecond duration to ticks.
func Milliseconds(ms int64) Ticks {
	return Ticks(ms * TicksPerSecond / 1000)
}

// MillisecondsF converts a millisecond duration to ticks.
func MillisecondsF(ms float64) Ticks {
	return Ticks(ms*float64(TicksPerSecond)/1000.0 + 0.5)
}

// Milliseconds renders the tick span as whole milliseconds (rounded).
func (t Ticks) Milliseconds() int64 {
	if t >= 0 {
		return (int64(t)*1000 + TicksPerSecond/2) / TicksPerSecond
	}
	return -(((-int64(t))*1000 + TicksPerSecond/2) / TicksPerSecond)
}

// Seconds renders the tick span as a floating point second count.
func (t Ticks) Seconds() float64 {
	return float64(t) / float64(TicksPerSecond)
}

// FromRTP maps a value expressed in an RTP clock rate onto the unified clock.
func FromRTP(value uint64, rate uint32) Ticks {
	if rate == 0 {
		return 0
	}
	return Ticks(int64(value) * TicksPerSecond / int64(rate))
}

// ToRTP maps unified ticks onto an RTP clock rate.
func ToRTP(t Ticks, rate uint32) uint64 {
	if rate == 0 {
		return 0
	}
	return uint64(int64(t) * int64(rate) / TicksPerSecond)
}

// Add combines two tick spans.
func (t Ticks) Add(d Ticks) Ticks { return t + d }

// String renders ticks for logs.
func (t Ticks) String() string {
	return fmt.Sprintf("%dms", t.Milliseconds())
}
