// Package timeline defines the single nanosecond time base every subsystem
// uses, plus the deterministic rounding rules that keep repeated runs of the
// same input bit-identical in their manifests.
package timeline

import "math"

// Clock converts raw Matroska timecode units into absolute nanoseconds.
// One Matroska timecode unit equals TimestampScale nanoseconds; the default
// scale (1,000,000) makes one unit exactly one millisecond.
type Clock struct {
	scaleNS uint64
}

// DefaultTimestampScale is the Matroska default when TimestampScale is absent.
const DefaultTimestampScale uint64 = 1_000_000

// NewClock builds a clock from a raw TimestampScale value. A zero value is
// treated as the specification default rather than as division by zero.
func NewClock(timestampScale uint64) Clock {
	if timestampScale == 0 {
		timestampScale = DefaultTimestampScale
	}
	return Clock{scaleNS: timestampScale}
}

// ScaleNS returns the clock's nanoseconds-per-timecode-unit.
func (c Clock) ScaleNS() uint64 { return c.scaleNS }

// ToNanoseconds maps a raw timecode to an absolute nanosecond position.
// Timecodes may be negative (block-relative offsets), so the result is signed.
func (c Clock) ToNanoseconds(timecode int64) int64 {
	return timecode * int64(c.scaleNS)
}

// ToMillis converts nanoseconds to fractional milliseconds for reports.
func ToMillis(ns int64) float64 { return float64(ns) / 1e6 }

// RoundMillis applies the archival rounding rule: half away from zero,
// never banker's rounding, so the same ns value always prints the same ms.
func RoundMillis(ns int64) int64 {
	if ns >= 0 {
		return (ns + 500_000) / 1_000_000
	}
	return -((-ns + 500_000) / 1_000_000)
}

// AlignmentErrorMS returns the absolute difference between actual and
// expected nanosecond positions, expressed in milliseconds for QA reports.
func AlignmentErrorMS(actualNS, expectedNS int64) float64 {
	d := actualNS - expectedNS
	if d < 0 {
		d = -d
	}
	return math.Round(float64(d)/1e3) / 1e3 // keep 3 decimal places deterministically
}
