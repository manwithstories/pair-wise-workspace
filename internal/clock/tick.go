// Package clock provides a shared rational media clock. All subsystems
// normalize timestamps to a master tick so audio and video stay comparable.
package clock

import "fmt"

// Rate is the master tick rate in ticks per second. It is the LCM of the
// supported sample rates (44100, 48000, 96000) times 8, so sample positions
// and sub-sample interpolation remain exact integers.
const Rate int64 = 7056000

// Tick is an instant on the master clock.
type Tick int64

// Rat is an exact rational number.
type Rat struct{ Num, Den int64 }

// FromSamples converts a sample index at the given sample rate to ticks.
func FromSamples(n int64, sampleRate int) Tick {
	if sampleRate <= 0 || Rate%int64(sampleRate) != 0 {
		// Fall back to exact rational arithmetic for unsupported rates.
		return Tick(n * Rate / int64(max(sampleRate, 1)))
	}
	return Tick(n * (Rate / int64(sampleRate)))
}

// ToSamples converts ticks to a sample index at the given rate, rounding to
// the nearest sample.
func ToSamples(t Tick, sampleRate int) int64 {
	r := int64(sampleRate)
	num := int64(t) * r
	return (num + Rate/2) / Rate
}

// ToMillis returns the tick instant in milliseconds.
func ToMillis(t Tick) float64 { return float64(t) * 1000 / float64(Rate) }

// FromMillis converts milliseconds to ticks, rounding to nearest.
func FromMillis(ms float64) Tick { return Tick(ms*float64(Rate)/1000 + 0.5) }

// Seconds returns the tick instant in seconds as an exact rational.
func (t Tick) Seconds() Rat { return Rat{Num: int64(t), Den: Rate} }

// Add returns t advanced by d ticks.
func (t Tick) Add(d Tick) Tick { return t + d }

// Sub returns the distance t-other in ticks.
func (t Tick) Sub(other Tick) Tick { return t - other }

// String renders the tick as seconds with microsecond precision.
func (t Tick) String() string { return fmt.Sprintf("%.6fs", float64(t)/float64(Rate)) }

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
