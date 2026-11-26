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
}

// NewWrapExpander validates the period and builds an expander.
func NewWrapExpander(streamID int, period int64) (*WrapExpander, error) {
	if period <= 0 {
		return nil, fmt.Errorf("%w: period=%d", ErrBadPeriod, period)
	}
	return &WrapExpander{streamID: streamID, period: period, half: period / 2}, nil
}

// Expand maps one raw PTS onto the unwrapped axis. Equal raw values map to
// the same expanded value (duplicates stay visible to the transport layer).
// A backward step bigger than half the period adds exactly one period.
// The returned WrapEvent is non-nil whenever a wrap was detected.
func (w *WrapExpander) Expand(rawPTS, sourceOffset int64) (int64, *WrapEvent, error) {
	if rawPTS < 0 {
		return 0, nil, fmt.Errorf("negative raw PTS %d on stream %d", rawPTS, w.streamID)
	}
	if !w.started {
		w.started = true
		w.prevRaw = rawPTS
		w.lastFull = rawPTS
		return rawPTS, nil, nil
	}

	var event *WrapEvent
	full := w.lastFull
	if rawPTS+w.half < w.prevRaw {
		// Backward jump across more than half the counter range: wrap.
		w.wraps++
		full, err := add64(w.lastFull, w.period)
		if err != nil {
			return 0, nil, err
		}
		full = full - w.prevRaw + rawPTS
		event = &WrapEvent{
			StreamID:     w.streamID,
			WrapCount:    w.wraps,
			RawPTS:       rawPTS,
			PrevRawPTS:   w.prevRaw,
			UnwrappedPTS: full,
			SourceOffset: sourceOffset,
		}
		w.lastFull = full
		w.prevRaw = rawPTS
		return full, event, nil
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
	return full, event, nil
}

// Wraps reports how many wraparounds have been expanded so far.
func (w *WrapExpander) Wraps() int { return w.wraps }

// Started reports whether any sample has been expanded.
func (w *WrapExpander) Started() bool { return w.started }
