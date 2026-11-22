// Package audio normalises decoded audio to a common sample rate, channel
// layout and loudness target. Resampling and loudness measurement share the
// canonical media clock and the structured fault model; segments that cannot
// be decoded leave an explicit timeline gap instead of silence.
package audio

import (
	"errors"
	"fmt"

	"mediad/internal/clock"
)

// CanonicalSampleRate is the delivery sample rate.
const CanonicalSampleRate = 48000

// Source is one decoded audio segment.
type Source struct {
	URI              string
	RepresentationID string
	StartTicks       int64 // canonical
	DurationTicks    int64 // canonical, used for gaps
	SampleRate       int
	Channels         int
	Samples          []float64 // interleaved, range [-1,1]
	Decodable        bool
}

// Target is the delivery format.
type Target struct {
	SampleRate      int
	Channels        int
	LoudnessLUFS    float64 // integrated target, e.g. -23 (EBU R128)
	PeakCeilingDBFS float64 // maximum true peak after gain
}

// DefaultTarget matches EBU R128 delivery: 48 kHz stereo at -23 LUFS.
func DefaultTarget() Target {
	return Target{SampleRate: CanonicalSampleRate, Channels: 2, LoudnessLUFS: -23, PeakCeilingDBFS: -1}
}

// Validate checks target ranges.
func (t Target) Validate() error {
	if t.SampleRate != 48000 {
		return fmt.Errorf("audio: target sample rate %d is not 48000", t.SampleRate)
	}
	if t.Channels != 1 && t.Channels != 2 {
		return fmt.Errorf("audio: target channels %d unsupported", t.Channels)
	}
	if t.LoudnessLUFS > 0 || t.LoudnessLUFS < -70 {
		return fmt.Errorf("audio: loudness target %.1f out of range", t.LoudnessLUFS)
	}
	if t.PeakCeilingDBFS > 0 || t.PeakCeilingDBFS < -60 {
		return fmt.Errorf("audio: peak ceiling %.1f out of range", t.PeakCeilingDBFS)
	}
	return nil
}

// Result is the normalised segment. When Gap is true the timeline keeps the
// segment position but no samples are emitted.
type Result struct {
	URI              string
	RepresentationID string
	StartTicks       int64
	DurationTicks    int64
	SampleRate       int
	Channels         int
	Samples          []float64
	MeasuredLUFS     float64
	GainDB           float64
	PeakDBFS         float64
	Gap              bool
}

// ErrNotDecodable marks a segment that cannot be normalised.
var ErrNotDecodable = errors.New("audio: segment not decodable")

// SampleDurationTicks converts a frame count at rate to canonical ticks.
func SampleDurationTicks(frames, rate int) int64 {
	if frames <= 0 || rate <= 0 {
		return 0
	}
	tb := clock.Default()
	return tb.TicksFromDuration(0) + divideRound(int64(frames)*int64(tb.Timescale), int64(rate))
}

// TicksToFrames converts canonical ticks to frames at rate.
func TicksToFrames(ticks int64, rate int) int {
	if ticks <= 0 || rate <= 0 {
		return 0
	}
	return int(divideRound(ticks*int64(rate), int64(clock.DefaultTimescale)))
}

func divideRound(n, d int64) int64 {
	if d == 0 {
		return 0
	}
	if (n < 0) != (d < 0) {
		return (n - d/2) / d
	}
	return (n + d/2) / d
}

// FrameCount returns the number of frames in an interleaved buffer.
func FrameCount(samples []float64, channels int) int {
	if channels <= 0 {
		return 0
	}
	return len(samples) / channels
}

// DurationTicks returns the exact canonical duration of a buffer.
func DurationTicks(samples []float64, rate, channels int) int64 {
	return SampleDurationTicks(FrameCount(samples, channels), rate)
}
