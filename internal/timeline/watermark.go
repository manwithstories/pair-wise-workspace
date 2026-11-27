// Package timeline advances the event-time watermark and replays
// affected output deterministically when late data arrives inside the
// rewind window.
package timeline

// Watermark is a monotone event-time watermark with a rewind window.
type Watermark struct {
	current int64
	set     bool
	window  int64 // rewind window in nanoseconds
}

func NewWatermark(windowNanos int64) *Watermark {
	return &Watermark{window: windowNanos}
}

// Advance moves the watermark forward. A timestamp at or behind the
// current watermark leaves it unchanged and reports false, keeping the
// watermark monotone.
func (w *Watermark) Advance(t int64) bool {
	if !w.set || t > w.current {
		w.current = t
		w.set = true
		return true
	}
	return false
}

// Current returns the watermark and whether it has been set.
func (w *Watermark) Current() (int64, bool) { return w.current, w.set }

// Window returns the rewind window in nanoseconds.
func (w *Watermark) Window() int64 { return w.window }

// InWindow reports whether t is still inside the rewind window.
func (w *Watermark) InWindow(t int64) bool {
	if !w.set {
		return true
	}
	return t >= w.current-w.window
}

// Restore sets the watermark from checkpoint state.
func (w *Watermark) Restore(current int64, set bool) {
	w.current, w.set = current, set
}
