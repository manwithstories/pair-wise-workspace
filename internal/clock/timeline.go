// Package clock owns the single 90 kHz media timescale used by audio, RTP,
// video and the report. All conversions are integer-only so repeated runs are
// bit-for-bit deterministic.
package clock

import "sort"

// Timescale is the negotiated media clock rate (90 kHz, the RTP video clock).
const Timescale int64 = 90000

// Tick is a media timestamp in the 90 kHz timescale.
type Tick int64

// FromMillis converts wall-clock milliseconds to ticks, rounding to nearest.
func FromMillis(ms float64) Tick {
	if ms >= 0 {
		return Tick(ms*float64(Timescale)/1000 + 0.5)
	}
	return Tick(ms*float64(Timescale)/1000 - 0.5)
}

// Millis returns ticks as milliseconds.
func (t Tick) Millis() float64 { return float64(t) * 1000 / float64(Timescale) }

// Seconds returns ticks as seconds.
func (t Tick) Seconds() float64 { return float64(t) / float64(Timescale) }

// Rescale converts value from one timescale to another using exact integer
// arithmetic. It returns the truncated quotient and the remainder, which lets
// callers accumulate rounding error without drifting.
func Rescale(value, from, to int64) (out, rem int64) {
	if from <= 0 || to <= 0 {
		return 0, 0
	}
	prod := value * to
	return prod / from, prod % from
}

// SamplesToTicks converts a sample count at sampleRate to 90 kHz ticks.
func SamplesToTicks(samples int64, sampleRate int) (Tick, int64) {
	out, rem := Rescale(samples, int64(sampleRate), Timescale)
	return Tick(out), rem
}

// TicksToSamples converts ticks to a sample count at sampleRate.
func TicksToSamples(t Tick, sampleRate int) (int64, int64) {
	return Rescale(int64(t), Timescale, int64(sampleRate))
}

// SamplesForDuration returns the sample count for a duration in ticks,
// rounding to nearest so callers never lose a frame at block boundaries.
func SamplesForDuration(d Tick, sampleRate int) int64 {
	n, rem := Rescale(int64(d), Timescale, int64(sampleRate))
	if rem*2 >= Timescale {
		n++
	}
	return n
}

// Span is a half-open media interval [Start, End).
type Span struct {
	Start Tick
	End   Tick
}

// Len returns the span duration in ticks.
func (s Span) Len() Tick { return s.End - s.Start }

// MonotonicError describes the first non-monotonic segment in a list.
type MonotonicError struct {
	Index int
	Prev  Span
	Cur   Span
}

// CheckMonotonic verifies that spans are ordered, non-negative and do not
// overlap. It returns nil for an empty list.
func CheckMonotonic(spans []Span) *MonotonicError {
	var prev Span
	for i, cur := range spans {
		if cur.End < cur.Start || cur.Start < 0 {
			return &MonotonicError{Index: i, Cur: cur}
		}
		if i > 0 && cur.Start < prev.End {
			return &MonotonicError{Index: i, Prev: prev, Cur: cur}
		}
		prev = cur
	}
	return nil
}

// Percentile returns the nearest-rank percentile (p in [0,1]) of values.
// It sorts a copy and never mutates the caller's slice.
func Percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	if p <= 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	cp := append([]float64(nil), values...)
	sort.Float64s(cp)
	rank := int(float64(len(cp))*p+0.999999) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(cp) {
		rank = len(cp) - 1
	}
	return cp[rank]
}
