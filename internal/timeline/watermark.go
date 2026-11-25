// Package timeline advances event-time watermarks and deterministically
// replays late events inside a bounded retention window.
package timeline

// Watermark is a monotone event-time frontier. It never moves backwards.
type Watermark struct {
	allowanceMS int64
	currentMS   int64
	have        bool
}

// NewWatermark creates a watermark delayed by allowanceMS.
func NewWatermark(allowanceMS int64) *Watermark {
	if allowanceMS < 0 {
		allowanceMS = 0
	}
	return &Watermark{allowanceMS: allowanceMS}
}

// Observe records an event time and returns the resulting watermark.
func (w *Watermark) Observe(eventMS int64) (int64, bool) {
	candidate := eventMS - w.allowanceMS
	if candidate < 0 {
		candidate = 0
	}
	if !w.have {
		w.have = true
		w.currentMS = candidate
		return w.currentMS, true
	}
	if candidate > w.currentMS {
		w.currentMS = candidate
		return w.currentMS, true
	}
	return w.currentMS, false
}

// Current returns the latest monotone watermark.
func (w *Watermark) Current() int64 { return w.currentMS }

// Have reports whether any event has initialized the watermark.
func (w *Watermark) Have() bool { return w.have }

// IsLate reports whether eventMS is behind the current frontier.
func (w *Watermark) IsLate(eventMS int64) bool {
	return w.have && eventMS < w.currentMS
}
