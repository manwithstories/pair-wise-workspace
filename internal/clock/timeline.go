// Package clock defines the unified integer-nanosecond timeline shared by
// all tracks, plus conversion helpers for per-track sample clocks and
// 32-bit RTP timestamp wraparound handling.
package clock

import (
	"errors"
	"fmt"
	"math"
)

// Time is an integer nanosecond offset on the unified timeline.
type Time int64

const (
	NsPerMs  int64 = 1_000_000
	NsPerSec int64 = 1_000_000_000
)

// ErrOverflow is returned when a sample count cannot be represented in ns.
var ErrOverflow = errors.New("clock: timestamp conversion overflow")

// Scale converts a non-negative sample count at the given clock rate to
// nanoseconds. Negative durations, zero rates and arithmetic overflow are
// reported as errors rather than silently wrapped.
func Scale(samples int64, rate uint32) (int64, error) {
	if rate == 0 {
		return 0, fmt.Errorf("clock: zero sample rate")
	}
	if samples < 0 {
		return 0, fmt.Errorf("clock: negative duration %d", samples)
	}
	if samples > math.MaxInt64/NsPerSec {
		return 0, fmt.Errorf("%w: samples=%d rate=%d", ErrOverflow, samples, rate)
	}
	return samples * NsPerSec / int64(rate), nil
}

// Duration validates and returns end-start as nanoseconds.
func Duration(start, end Time) (int64, error) {
	d := int64(end) - int64(start)
	if d < 0 {
		return 0, fmt.Errorf("clock: negative duration %d (start=%d end=%d)", d, start, end)
	}
	return d, nil
}

// reorderSlack is the tolerated backward jitter in extended ticks before a
// timestamp is reported as non-monotonic (~11.6ms at 90kHz).
const reorderSlack = 1 << 20

// WrapTracker maps 32-bit RTP timestamps of one track onto an extended
// 64-bit sample counter, surviving wraparound and bounded reordering.
type WrapTracker struct {
	rate    uint32
	epoch   int64 // number of wraps observed
	last    uint32
	lastExt int64
	init    bool
}

// NewWrapTracker returns a tracker for the given sample clock rate.
func NewWrapTracker(rate uint32) *WrapTracker { return &WrapTracker{rate: rate} }

// Convert maps ts to the unified ns timeline. It extends the counter across
// wraparound, tolerates small out-of-order delivery, and reports
// non-monotonic jumps beyond the reorder slack.
func (w *WrapTracker) Convert(ts uint32) (int64, error) {
	if w.rate == 0 {
		return 0, fmt.Errorf("clock: zero sample rate")
	}
	if !w.init {
		w.init = true
		w.last = ts
		w.lastExt = int64(ts)
		return Scale(int64(ts), w.rate)
	}
	epoch := w.epoch
	if ts >= w.last {
		if ts-w.last > 1<<31 {
			epoch-- // late packet emitted before the wrap
		}
	} else if w.last-ts > 1<<31 {
		epoch++ // forward wraparound
	}
	if epoch < 0 {
		return 0, fmt.Errorf("clock: timestamp %d precedes stream epoch", ts)
	}
	if epoch > w.epoch || (epoch == w.epoch && ts >= w.last && ts-w.last <= 1<<31) {
		w.epoch = epoch
		w.last = ts
	}
	ext := epoch<<32 | int64(ts)
	if ext < w.lastExt-reorderSlack {
		return 0, fmt.Errorf("clock: non-monotonic timestamp: ext=%d last=%d", ext, w.lastExt)
	}
	if ext > w.lastExt {
		w.lastExt = ext
	}
	return Scale(ext, w.rate)
}

// Snapshot returns the wrap state for checkpointing.
func (w *WrapTracker) Snapshot() (epoch int64, last uint32, lastExt int64, init bool) {
	return w.epoch, w.last, w.lastExt, w.init
}

// Restore reinstates a previously snapshotted wrap state.
func (w *WrapTracker) Restore(epoch int64, last uint32, lastExt int64, init bool) {
	w.epoch, w.last, w.lastExt, w.init = epoch, last, lastExt, init
}
