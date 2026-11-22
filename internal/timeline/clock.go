// Package timeline maps every track onto a single nanosecond base.
// All arithmetic is integer nanoseconds; no floating point is ever
// accumulated.
package timeline

// Nanos is the unified time base: signed integer nanoseconds.
type Nanos int64

const (
	Nanosecond  Nanos = 1
	Millisecond       = Nanos(1_000_000)
	Second            = Nanos(1_000_000_000)
)

// Scale converts integer ticks to nanoseconds: ns = ticks * Num / Den.
// The division is exact for the timebases used here; any remainder is
// truncated toward zero, never rounded through a float.
type Scale struct {
	Num int64
	Den int64
}

// ToNanos converts ticks to the unified base.
func (s Scale) ToNanos(ticks int64) Nanos {
	return Nanos(ticks * s.Num / s.Den)
}

// NanosPerTick returns a scale for a Matroska-style TimeScale value
// (nanoseconds per tick).
func NanosPerTick(nsPerTick uint64) Scale {
	return Scale{Num: int64(nsPerTick), Den: 1}
}
