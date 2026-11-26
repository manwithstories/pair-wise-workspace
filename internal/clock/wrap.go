package clock

import (
	"errors"
	"fmt"
)

// ErrBadPeriod is returned for a non-positive wrap period.
var ErrBadPeriod = errors.New("wrap period must be positive")

// WrapEvent records one timestamp wraparound observed on a stream. The
// pipeline keeps both the supplier's raw value and the expanded monotonic
// value; nothing is rewritten in place.
type WrapEvent struct {
	StreamID      int   `json:"stream_id"`
	WrapCount     int   `json:"wrap_count"`
	RawPTS        int64 `json:"raw_pts"`
	PrevRawPTS    int64 `json:"prev_raw_pts"`
	UnwrappedPTS  int64 `json:"unwrapped_pts"`
	SourceOffset  int64 `json:"source_offset"`
}

// WrapExpander turns a modular counter (period == 2^k typically) into a
// monotonically non-decreasing integer series. It is stateful per stream
// and never mutates the input value.
type WrapExpander struct {
	streamID int
	period   int64
	// half is the backward-jump threshold: any drop larger than half the
	// period is a wraparound; a smaller drop is a genuine out-of-order or
	// duplicate sample and must not be reinterpreted.
	half       int64
	wraps      int
	prevRaw    int64
	lastFull   int64
	started    bool
	// base is the counter's value at the stream's zero-phase origin. Every
	// expanded value is reduced by it so the unified timeline starts near
	// zero even when the supplier counter begins at a non-zero PTS.
	base       int64
	baseSet    bool
}

// NewWrapExpander validates the period and builds an expander with a zero
// baseline (the first observed raw value becomes the origin).
func NewWrapExpander(streamID int, period int64) (*WrapExpander, error) {
	if period <= 0 {
		return nil, fmt.Errorf("%w: period=%d", ErrBadPeriod, period)
	}
	return &WrapExpander{streamID: streamID, period: period, half: period / 2}, nil
}

// NewWrapExpanderWithBase builds an expander whose unwrapped axis is reduced
// by baseTicks, anchoring a counter with a known non-zero origin (the stream's
// start PTS) onto the zero-phase master timeline.
func NewWrapExpanderWithBase(streamID int, period, baseTicks int64) (*WrapExpander, error) {
	w, err := NewWrapExpander(streamID, period)
	if err != nil {
		return nil, err
	}
	w.base = baseTicks
	w.baseSet = true
	return w, nil
}

// Expand maps one raw PTS onto the unwrapped axis. Equal raw values map to
// the same expanded value (duplicates stay visible to the transport layer).
// A backward step bigger than half the period adds exactly one period. When
// the expander carries a base, the returned axis is reduced by that base so
// it begins at the zero-phase origin. The returned WrapEvent is non-nil
// whenever a wrap was detected.
func (w *WrapExpander) Expand(rawPTS, sourceOffset int64) (int64, *WrapEvent, error) {
	if rawPTS < 0 {
		return 0, nil, fmt.Errorf("negative raw PTS %d on stream %d", rawPTS, w.streamID)
	}
	if !w.started {
		w.started = true
		w.prevRaw = rawPTS
		w.lastFull = rawPTS
		if !w.baseSet {
			w.base = rawPTS // zero-baseline expander: first sample is origin
		}
		return rawPTS - w.base, nil, nil
	}

	var event *WrapEvent
	full := w.lastFull
	if rawPTS+w.half < w.prevRaw {
		// Backward jump across more than half the counter range: wrap.
		w.wraps++
		next, err := add64(w.lastFull, w.period)
		if err != nil {
			return 0, nil, err
		}
		full = next - w.prevRaw + rawPTS
		event = &WrapEvent{
			StreamID:     w.streamID,
			WrapCount:    w.wraps,
			RawPTS:       rawPTS,
			PrevRawPTS:   w.prevRaw,
			UnwrappedPTS: full - w.base,
			SourceOffset: sourceOffset,
		}
		w.lastFull = full
		w.prevRaw = rawPTS
		return full - w.base, event, nil
	}

	// Forward step, duplicate, or small jitter: preserve monotonicity.
	delta := rawPTS - w.prevRaw
	if delta > 0 {
		next, err := add64(w.lastFull, delta)
		if err != nil {
			return 0, nil, err
		}
		full = next
	}
	w.lastFull = full
	w.prevRaw = rawPTS
	return full - w.base, event, nil
}

// Wraps reports how many wraparounds have been expanded so far.
func (w *WrapExpander) Wraps() int { return w.wraps }

// Started reports whether any sample has been expanded.
func (w *WrapExpander) Started() bool { return w.started }
