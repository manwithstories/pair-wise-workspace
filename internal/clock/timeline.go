// Package clock implements the shared rational time base and the monotonic
// 90 kHz media timeline used by every pipeline module.
//
// All wall-clock style timestamps in the index are expressed in 90 kHz ticks
// (one tick = 1/90000 s). Source streams arrive in their own rational time
// bases; this package converts them into the shared clock and makes sure that
// a timestamp wraparound or a PTS reset can never move a cut point backwards.
package clock

import (
	"errors"
	"math"
)

// TicksPerSecond is the shared media-clock frequency.
const TicksPerSecond int64 = 90000

// Rational mirrors an FFmpeg-style rational time base num/den without
// importing CGo types, which keeps this package trivially testable.
type Rational struct {
	Num int
	Den int
}

// NewRational builds a Rational and rejects a zero denominator.
func NewRational(num, den int) (Rational, error) {
	if den == 0 {
		return Rational{}, errors.New("clock: rational has zero denominator")
	}
	return Rational{Num: num, Den: den}, nil
}

// Float64 returns the value as a floating point number.
func (r Rational) Float64() float64 {
	if r.Den == 0 {
		return 0
	}
	return float64(r.Num) / float64(r.Den)
}

// ToTicks converts a timestamp expressed in time base tb into 90 kHz ticks,
// rounding to the nearest tick.
func ToTicks(ts int64, tb Rational) int64 {
	if tb.Den == 0 {
		return 0
	}
	// ts * Num / Den * 90000, computed in 128-bit-safe float then rounded.
	sec := float64(ts) * float64(tb.Num) / float64(tb.Den)
	return int64(math.Round(sec * float64(TicksPerSecond)))
}

// TicksToRational converts shared-clock ticks into a timestamp in time base
// tb, rounding to the nearest source tick.
func TicksToRational(ticks int64, tb Rational) int64 {
	if tb.Num == 0 {
		return 0
	}
	sec := float64(ticks) / float64(TicksPerSecond)
	return int64(math.Round(sec * float64(tb.Den) / float64(tb.Num)))
}

// Ms converts ticks to milliseconds (rounded).
func Ms(ticks int64) int64 {
	return int64(math.Round(float64(ticks) * 1000.0 / float64(TicksPerSecond)))
}

// FromMs converts milliseconds to ticks.
func FromMs(ms int64) int64 {
	return int64(math.Round(float64(ms) / 1000.0 * float64(TicksPerSecond)))
}

// wrapThreshold treats a forward jump larger than this as a reset and a
// backward move smaller than this as a possible integer wraparound. Streams
// are short relative to 33-bit space, so a generous bound is safe.
const wrapThreshold int64 = 1 << 33

// Timeline turns per-segment source timestamps into a monotonically
// non-decreasing sequence of shared-clock ticks. It is single-goroutine; the
// pipeline serialises access per track.
type Timeline struct {
	lastTicks   int64
	lastRaw     int64
	started     bool
	offset      int64 // accumulated shift applied to the raw timeline
	resets      int
	wraparounds int
}

// NewTimeline returns an empty Timeline.
func NewTimeline() *Timeline { return &Timeline{} }

// Advance maps the next raw source timestamp (in shared ticks already) to a
// monotonic tick. It distinguishes a wraparound (counter rolled over its
// integer range) from an explicit PTS reset (a small backward jump) and never
// returns a value lower than the previous one.
func (t *Timeline) Advance(raw int64) int64 {
	if !t.started {
		t.started = true
		t.lastRaw = raw
		t.lastTicks = raw
		return raw
	}

	delta := raw - t.lastRaw
	switch {
	case delta < -wrapThreshold:
		// Forward wraparound: the raw counter rolled over its range.
		t.wraparounds++
		t.offset += wrapThreshold * 2
	case delta < 0:
		// Backward move that is not a full wraparound: an explicit PTS reset.
		t.resets++
		// Shift so the new value continues directly after the previous tick.
		t.offset = t.lastTicks + 1 - raw
	default:
		// Normal forward progress.
	}

	adjusted := raw + t.offset
	if adjusted < t.lastTicks {
		adjusted = t.lastTicks
	}
	t.lastRaw = raw
	t.lastTicks = adjusted
	return adjusted
}

// Resets reports how many explicit PTS resets were normalised.
func (t *Timeline) Resets() int { return t.resets }

// Wraparounds reports how many integer wraparounds were normalised.
func (t *Timeline) Wraparounds() int { return t.wraparounds }

// Last returns the most recent adjusted tick.
func (t *Timeline) Last() int64 { return t.lastTicks }

// AlignVideo rounds a tick down to the nearest video frame boundary.
func AlignVideo(ticks int64, frameRate Rational) int64 {
	if frameRate.Num <= 0 || frameRate.Den <= 0 {
		return ticks
	}
	frameTicks := TicksPerSecond * int64(frameRate.Den) / int64(frameRate.Num)
	if frameTicks <= 0 {
		return ticks
	}
	return (ticks / frameTicks) * frameTicks
}

// AlignAudio rounds a tick down to the nearest audio sample boundary.
func AlignAudio(ticks int64, sampleRate int) int64 {
	if sampleRate <= 0 {
		return ticks
	}
	// ticks per sample, rounded; floor division on the sample index then back.
	sampleIndex := ticks * int64(sampleRate) / TicksPerSecond
	return sampleIndex * TicksPerSecond / int64(sampleRate)
}
