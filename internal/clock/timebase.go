// Package clock defines the shared 90 kHz media timebase and the exact
// conversion/rounding rules used by every subsystem so that timestamps can
// never drift between the demuxer, timeline and remuxer.
package clock

import "math"

// Rate is the RTP/ISO BMFF video clock frequency shared by all tracks.
// Audio track timescales are converted into this single rate on ingest.
const Rate uint32 = 90_000

// Tick is one unit of the unified timebase (1/90000 s).
type Tick int64

// FromTimescale converts a duration expressed in an arbitrary track timescale
// into unified 90 kHz ticks using round-half-away-from-zero, which keeps
// repeated conversions stable and free of cumulative drift.
func FromTimescale(units uint64, timescale uint32) Tick {
	if timescale == 0 {
		return 0
	}
	num := float64(units) * float64(Rate)
	q := num / float64(timescale)
	return Tick(math.Round(q))
}

// ToTimescale converts unified ticks back into a target timescale with the
// same rounding rule, so FromTimescale(ToTimescale(x)) == x for exact values.
func ToTimescale(t Tick, timescale uint32) uint64 {
	if timescale == 0 || t == 0 {
		return 0
	}
	q := float64(t) * float64(timescale) / float64(Rate)
	return uint64(math.Round(math.Abs(q)))
}

// FromMillis converts milliseconds into unified ticks.
func FromMillis(ms int64) Tick {
	return Tick(math.Round(float64(ms) * float64(Rate) / 1000.0))
}

// ToMillis converts unified ticks into milliseconds.
func ToMillis(t Tick) int64 {
	return int64(math.Round(float64(t) * 1000.0 / float64(Rate)))
}

// SnapSample moves a requested boundary to the nearest timestamp that keeps a
// whole compressed sample. It returns the snapped boundary and the signed
// deviation in ticks (snapped minus requested).
func SnapSample(requested Tick, starts []Tick) (snapped Tick, deviation Tick) {
	if len(starts) == 0 {
		return requested, 0
	}
	best := starts[0]
	for _, s := range starts {
		if absTick(s-requested) < absTick(best-requested) {
			best = s
		}
	}
	return best, best - requested
}

func absTick(t Tick) Tick {
	if t < 0 {
		return -t
	}
	return t
}
