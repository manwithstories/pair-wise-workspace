// Package media defines the unified cross-track media clock and the
// shared fault model used by the ingest, jitter, recovery, timeline and
// normalization stages.
package media

import "fmt"

// NanosPerSecond is the resolution of the unified timeline.
const NanosPerSecond = int64(1_000_000_000)

// MediaTime is an instant on the unified timeline. The whole-nanosecond
// part is complemented by a remainder expressed in units of the source
// clock, so conversions from heterogeneous RTP clock rates do not
// silently accumulate drift over long sessions.
type MediaTime struct {
	Nanos int64 // whole nanoseconds on the unified timeline
	Rem   int64 // sub-nanosecond remainder, in units of 1/Rate second
	Rate  int64 // source clock rate the remainder refers to
}

// FromTicks converts a tick count on a clock running at rate ticks per
// second into a MediaTime, preserving the conversion remainder.
func FromTicks(ticks, rate int64) MediaTime {
	if rate <= 0 {
		rate = 1
	}
	secs := ticks / rate
	remTicks := ticks % rate
	remNanos := remTicks * NanosPerSecond
	return MediaTime{
		Nanos: secs*NanosPerSecond + remNanos/rate,
		Rem:   remNanos % rate,
		Rate:  rate,
	}
}

// AddNanos shifts t by d nanoseconds, keeping the source remainder.
func (t MediaTime) AddNanos(d int64) MediaTime {
	return MediaTime{Nanos: t.Nanos + d, Rem: t.Rem, Rate: t.Rate}
}

// Compare orders two media times, using the remainders to break ties
// between instants that share the same whole nanoseconds.
func Compare(a, b MediaTime) int {
	switch {
	case a.Nanos < b.Nanos:
		return -1
	case a.Nanos > b.Nanos:
		return 1
	}
	l := a.Rem * b.Rate
	r := b.Rem * a.Rate
	switch {
	case l < r:
		return -1
	case l > r:
		return 1
	}
	return 0
}

// Sub returns t-o in whole nanoseconds, dropping sub-nanosecond detail.
func (t MediaTime) Sub(o MediaTime) int64 { return t.Nanos - o.Nanos }

func (t MediaTime) String() string {
	n := t.Nanos
	if n < 0 {
		return "-" + MediaTime{Nanos: -n}.String()
	}
	return fmt.Sprintf("%d.%09d", n/NanosPerSecond, n%NanosPerSecond)
}

// MapTicks converts ticks from one clock rate to another, returning the
// whole converted ticks plus the leftover remainder in source units so
// callers keep exact accounts across rate changes instead of drifting.
func MapTicks(ticks, fromRate, toRate int64) (out, rem int64) {
	if fromRate <= 0 || toRate <= 0 {
		return 0, 0
	}
	num := ticks * toRate
	return num / fromRate, num % fromRate
}
