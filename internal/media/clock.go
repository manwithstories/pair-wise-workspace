package media

import "math"

// Clock90k is the shared RTP-style media time base: 90,000 ticks per second.
const Clock90k uint64 = 90_000

// Duration90k converts a millisecond duration to 90 kHz ticks.
func Duration90k(millis uint64) uint64 {
	return millis * Clock90k / 1_000
}

// ToMillis converts 90 kHz ticks to milliseconds (truncated).
func ToMillis(ticks uint64) uint64 {
	return ticks * 1_000 / Clock90k
}

// FromSamples maps a sample count at the given sample rate onto the 90 kHz
// clock using exact fractional arithmetic before rounding.
func FromSamples(samples, sampleRate uint64) uint64 {
	if sampleRate == 0 {
		return 0
	}
	return (samples*Clock90k + sampleRate/2) / sampleRate
}

// ToSamples maps 90 kHz ticks to a sample count at the given sample rate.
func ToSamples(ticks, sampleRate uint64) uint64 {
	return (ticks*sampleRate + Clock90k/2) / Clock90k
}

// StepForFPS returns the integer 90 kHz tick step for an integer frame rate
// and the remainder that must be distributed across successive frames so the
// long-run PTS drift stays below one frame interval.
func StepForFPS(fps uint64) (step uint64, remainder uint64) {
	if fps == 0 {
		return 0, 0
	}
	return Clock90k / fps, Clock90k % fps
}

// FractionalStep is a fractional 90 kHz PTS accumulator for frame rates that
// do not divide 90,000 evenly.
type FractionalStep struct {
	whole uint64
	num   uint64
	den   uint64
	acc   uint64
}

// NewFractionalStep builds an accumulator for fps using exact fractions.
func NewFractionalStep(fps uint64) FractionalStep {
	if fps == 0 {
		return FractionalStep{}
	}
	g := gcd(Clock90k, fps)
	return FractionalStep{
		whole: Clock90k / fps,
		num:   (Clock90k % fps) / g,
		den:   fps / g,
	}
}

// Next returns the number of ticks to advance for the next frame and updates
// the internal accumulator.
func (f *FractionalStep) Next() uint64 {
	f.acc += f.num
	extra := f.acc / f.den
	f.acc %= f.den
	return f.whole + extra
}

// SyncDiffMillis returns the absolute difference of two timestamps in
// milliseconds, safe against unsigned underflow.
func SyncDiffMillis(a, b uint64) uint64 {
	d := math.Abs(float64(a) - float64(b))
	return ToMillis(uint64(d))
}

func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
