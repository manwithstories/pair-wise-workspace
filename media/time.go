// Package media defines the unified time base, sample model and error
// taxonomy shared by the demux, segment, silence, fingerprint and output
// subsystems.
package media

import "fmt"

// Time is a rational timestamp: Value/Timescale seconds. Every track keeps
// its own timescale; conversions between tracks go through ToTimescale so
// cut points stay aligned on one time base.
type Time struct {
	Value     int64
	Timescale uint32
}

// NewTime validates and builds a Time.
func NewTime(value int64, timescale uint32) (Time, error) {
	if timescale == 0 {
		return Time{}, fmt.Errorf("media: zero timescale")
	}
	return Time{Value: value, Timescale: timescale}, nil
}

// ToTimescale converts t to another timescale, rounding toward zero.
func (t Time) ToTimescale(ts uint32) Time {
	if t.Timescale == ts {
		return t
	}
	v := t.Value * int64(ts) / int64(t.Timescale)
	return Time{Value: v, Timescale: ts}
}

// Seconds returns t as floating-point seconds (display/diagnostics only).
func (t Time) Seconds() float64 {
	return float64(t.Value) / float64(t.Timescale)
}

// Add returns t + d, where d is expressed in t's timescale.
func (t Time) Add(d int64) Time { return Time{Value: t.Value + d, Timescale: t.Timescale} }

// Compare returns -1, 0 or 1 comparing t to o on an absolute time base.
func (t Time) Compare(o Time) int {
	a := t.ToTimescale(o.Timescale)
	switch {
	case a.Value < o.Value:
		return -1
	case a.Value > o.Value:
		return 1
	}
	return 0
}

// Before reports whether t is strictly earlier than o.
func (t Time) Before(o Time) bool { return t.Compare(o) < 0 }

func (t Time) String() string {
	return fmt.Sprintf("%d/%d(%.3fs)", t.Value, t.Timescale, t.Seconds())
}
