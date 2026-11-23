// Package timeline provides a unified timebase for audio and video so that
// timestamps from nodes with different clocks can be compared and converted
// without drift.
package timeline

import (
	"fmt"
	"math/bits"
)

// Timescale is a number of ticks per second.
type Timescale int64

// Common timescales used across the pipeline.
const (
	Audio Timescale = 48000
	Video Timescale = 90000
	Millis Timescale = 1000
)

// Convert converts v ticks expressed in `from` into ticks of `to`,
// rounding to the nearest tick (half away from zero). It uses 128-bit
// intermediate arithmetic so media-scale values never overflow.
func Convert(v int64, from, to Timescale) int64 {
	if from <= 0 || to <= 0 {
		panic(fmt.Sprintf("timeline: invalid timescale %d -> %d", from, to))
	}
	if v == 0 || from == to {
		return v
	}
	neg := v < 0
	uv := uint64(v)
	if neg {
		uv = uint64(-v)
	}
	hi, lo := bits.Mul64(uv, uint64(to))
	q, r := bits.Div64(hi, lo, uint64(from))
	if 2*r >= uint64(from) {
		q++
	}
	if neg {
		return -int64(q)
	}
	return int64(q)
}

// ToMillis converts v ticks of the given timescale to whole milliseconds,
// rounding to nearest.
func ToMillis(v int64, ts Timescale) int64 { return Convert(v, ts, Millis) }

// FromMillis converts milliseconds to ticks of the given timescale.
func FromMillis(ms int64, ts Timescale) int64 { return Convert(ms, Millis, ts) }

// ToMillisExact converts ticks to fractional milliseconds (for metrics).
func ToMillisExact(v int64, ts Timescale) float64 {
	return float64(v) * 1000.0 / float64(ts)
}

// AbsDiffMillis returns the absolute difference, in milliseconds, between
// two timestamps expressed in different timescales.
func AbsDiffMillis(a int64, aTS Timescale, b int64, bTS Timescale) float64 {
	d := ToMillisExact(a, aTS) - ToMillisExact(b, bTS)
	if d < 0 {
		return -d
	}
	return d
}

// IsFrameAligned reports whether t (in ticks of ts) falls exactly on a
// boundary of frames with the given duration in ticks.
func IsFrameAligned(t int64, ts Timescale, frameDur int64) bool {
	if frameDur <= 0 {
		return false
	}
	return t%frameDur == 0
}
