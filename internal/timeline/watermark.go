// Package timeline tracks the event-time watermark, classifies late
// arrivals, and recomputes the canonical timeline deterministically.
package timeline

import "mediarchive/internal/clock"

// Verdict classifies an observed event time against the watermark.
type Verdict int

const (
	// OnTime events advance (or match) the watermark.
	OnTime Verdict = iota
	// Late events fall behind the watermark but inside the late window;
	// they trigger a replay recompute.
	Late
	// TooLate events fall beyond the late window; they are unrepairable.
	TooLate
)

// String names the verdict.
func (v Verdict) String() string {
	switch v {
	case OnTime:
		return "on_time"
	case Late:
		return "late"
	default:
		return "too_late"
	}
}

// Watermark tracks the maximum event time observed so far and classifies
// new observations against a configurable lateness window.
type Watermark struct {
	LateWindow clock.Time
	max        clock.Time
	init       bool
}

// NewWatermark creates a watermark with the given late window.
func NewWatermark(lateWindow clock.Time) *Watermark {
	return &Watermark{LateWindow: lateWindow}
}

// Observe classifies t and advances the watermark when t is new.
func (w *Watermark) Observe(t clock.Time) Verdict {
	if !w.init || t >= w.max {
		w.max = t
		w.init = true
		return OnTime
	}
	if w.max-t <= w.LateWindow {
		return Late
	}
	return TooLate
}

// Current returns the current watermark and whether it has been set.
func (w *Watermark) Current() (clock.Time, bool) {
	return w.max, w.init
}
