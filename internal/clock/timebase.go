// Package clock defines the shared integer 90 kHz time base used by every
// subsystem of avcore. All timestamps are int64 ticks at 90000 Hz; floating
// point is never used to accumulate time, so long conversions cannot drift.
package clock

import (
	"fmt"
	"math"
)

// TicksPerSecond is the common H.222 / MPEG-2 clock rate adopted across the
// whole pipeline: ffprobe events, cut points, captions and manifests all speak
// the same integer unit.
const TicksPerSecond int64 = 90_000

// Rate is an exact frame rate expressed as a rational number (frames per
// second), e.g. 30000/1001 for NTSC. Rates are never parsed from floats.
type Rate struct {
	Num int
	Den int
}

// Supported SMPTE rates. 24 and 23.976 share the 24-frame time code family.
var (
	Rate24     = Rate{Num: 24, Den: 1}
	Rate23976  = Rate{Num: 24000, Den: 1001}
	Rate25     = Rate{Num: 25, Den: 1}
	Rate30     = Rate{Num: 30, Den: 1}
	Rate2997   = Rate{Num: 30000, Den: 1001}
	Rate30000  = Rate{Num: 30000, Den: 1001} // canonical spelling of 29.97
)

// Valid reports whether the rate is a positive rational number.
func (r Rate) Valid() bool { return r.Num > 0 && r.Den > 0 }

// NominalFPS is the integer time-code frame count displayed per second, i.e.
// the rounded rate: 30 for 30000/1001, 25 for 25/1 and so on.
func (r Rate) NominalFPS() int { return (r.Num + r.Den/2) / r.Den }

// FramesPerDay returns the number of time code frames in 24 hours at the rate.
func (r Rate) FramesPerDay() int64 {
	return int64(r.Num) * 86400 / int64(r.Den)
}

// FrameDuration converts a whole frame count to 90 kHz ticks exactly
// (90000*Den/Num divides exactly for every supported SMPTE rate).
func (r Rate) FrameDuration(frames int64) int64 {
	return frames * TicksPerSecond * int64(r.Den) / int64(r.Num)
}

// TicksToFrame rounds to the nearest frame index at this rate.
func (r Rate) TicksToFrame(ticks int64) int64 {
	num := ticks * int64(r.Num)
	den := TicksPerSecond * int64(r.Den)
	if num >= 0 {
		return (num + den/2) / den
	}
	return -((-num + den/2) / den)
}

// Seconds converts a tick count to seconds; used only for display and for
// external tools whose interfaces are specified in seconds (ffmpeg -ss).
func Seconds(ticks int64) float64 {
	return float64(ticks) / float64(TicksPerSecond)
}

// FromSeconds turns an externally supplied second value into ticks, rounding
// to the nearest tick.
func FromSeconds(sec float64) int64 {
	return int64(math.Round(sec * float64(TicksPerSecond)))
}

// Millis converts milliseconds to ticks exactly.
func Millis(ms int64) int64 { return ms * TicksPerSecond / 1000 }

// ParseRate accepts the exact spellings used by the CSV time code feed:
// "24", "25", "30" and "30000/1001".
func ParseRate(s string) (Rate, error) {
	switch s {
	case "24":
		return Rate24, nil
	case "25":
		return Rate25, nil
	case "30":
		return Rate30, nil
	case "30000/1001":
		return Rate2997, nil
	default:
		return Rate{}, fmt.Errorf("clock: unsupported frame rate %q", s)
	}
}
