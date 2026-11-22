// Package clock provides the unified rational time base shared by the
// subtitle, ingest and timeline subsystems. All conversions use integer
// math only; no floating-point accumulation is allowed anywhere.
package clock

import "fmt"

// NanosPerSec is the resolution of the unified time base.
const NanosPerSec = int64(1_000_000_000)

// Rate is a rational frame rate: Num/Den frames per second.
type Rate struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

// Supported frame rates.
var (
	FPS24   = Rate{24, 1}
	FPS25   = Rate{25, 1}
	FPS30   = Rate{30, 1}
	FPS2997 = Rate{30000, 1001}
)

// Supported lists every rate the pipeline accepts.
var Supported = []Rate{FPS24, FPS25, FPS30, FPS2997}

func (r Rate) String() string { return fmt.Sprintf("%d/%d", r.Num, r.Den) }

// Valid reports whether r is one of the supported rates.
func (r Rate) Valid() bool {
	for _, s := range Supported {
		if s == r {
			return true
		}
	}
	return false
}

// NominalFPS is the frame-number bound for one second of timecode
// (e.g. 30 for 30000/1001).
func (r Rate) NominalFPS() int64 {
	n := r.Num / r.Den
	if r.Num%r.Den != 0 {
		n++
	}
	return n
}

// FrameToNanos converts an absolute frame index to nanoseconds using
// exact rational arithmetic, so long timelines never drift.
func (r Rate) FrameToNanos(f int64) int64 {
	q, rem := f/r.Num, f%r.Num
	return q*r.Den*NanosPerSec + rem*r.Den*NanosPerSec/r.Num
}

// NanosToFrame converts nanoseconds to a frame index (floor).
func (r Rate) NanosToFrame(ns int64) int64 {
	return ns * r.Num / (r.Den * NanosPerSec)
}
