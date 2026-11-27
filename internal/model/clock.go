// Package model defines the shared nanosecond time base and the byte-offset
// aware error model used by every mediad subsystem (index, transport, sync,
// pipeline, output).
package model

// Timestamp is the unified media time base: nanoseconds counted from the
// first sample of the first track in a container. All tracks and all
// cameras are aligned onto this single monotonic axis.
type Timestamp int64

// Nanoseconds per common time unit.
const (
	Nanosecond  Timestamp = 1
	Microsecond           = 1000 * Nanosecond
	Millisecond           = 1000 * Microsecond
	Second                = 1000 * Millisecond
)

// ToNanoseconds converts a duration expressed in timescale units onto the
// shared nanosecond base. Rounding is half-up so repeated conversions stay
// deterministic.
func ToNanoseconds(units uint64, timescale uint32) Timestamp {
	if timescale == 0 {
		return 0
	}
	return Timestamp((units*uint64(Second) + uint64(timescale)/2) / uint64(timescale))
}

// FromNanoseconds converts a shared-base duration back into timescale units.
func FromNanoseconds(ns Timestamp, timescale uint32) uint64 {
	if timescale == 0 {
		return 0
	}
	return (uint64(ns)*uint64(timescale) + uint64(Second)/2) / uint64(Second)
}

// DriftMs returns the absolute drift between two aligned timestamps in
// whole milliseconds.
func DriftMs(a, b Timestamp) int64 {
	d := int64(a - b)
	if d < 0 {
		d = -d
	}
	return d / int64(Millisecond)
}

// Unwrap resolves a modular timestamp against the previous unwrapped value.
// A forward jump smaller than half the modulus is treated as continuous;
// a large backward jump is treated as one wraparound. modulus is typically
// 1<<32 for RTP timestamps. Returns the unwrapped, monotonically growing
// value.
func Unwrap(prev, cur, modulus uint64) uint64 {
	if modulus == 0 || cur == prev {
		return cur
	}
	half := modulus / 2
	switch {
	case cur < prev && prev-cur > half:
		return cur + modulus
	case cur > prev && cur-prev > half:
		// wrapped backwards in the source stream (extreme clock skew)
		if cur >= modulus {
			return cur - modulus
		}
		return cur
	default:
		return cur
	}
}

// FirstRegression scans a PTS sequence and returns the index of the first
// sample whose PTS is strictly smaller than its predecessor. Returns -1
// when the sequence is non-decreasing.
func FirstRegression(pts []Timestamp) int {
	for i := 1; i < len(pts); i++ {
		if pts[i] < pts[i-1] {
			return i
		}
	}
	return -1
}
