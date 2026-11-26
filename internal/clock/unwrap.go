// Package clock normalises wrap-prone media timestamps and fits a per-camera
// drift model against sync anchors.
package clock

import "mediad/internal/media"

// Unwrapper tracks one camera track's raw timestamp stream and extends every
// value into the unwrapped 64-bit domain. Raw counters are folded into
// [0, 1<<width); a step forward that would cross the period rolls the epoch
// forward by exactly one period. Small backwards steps stay in the current
// epoch so late arrivals keep a truthful, slightly-behind timestamp.
type Unwrapper struct {
	width  int
	period int64
	last   int64 // last unwrapped value
	epoch  int64 // multiple of period added to folded raw values
	seen   bool
	wraps  int
}

// NewUnwrapper builds an unwrapper for a wrap width in bits. width must be in
// [16,62]; the period is 1<<width.
func NewUnwrapper(width int) (*Unwrapper, error) {
	if width < 1 || width > 62 {
		return nil, media.NewError(media.ErrInvalidTimebase, "wrap width %d outside [1,62]", width)
	}
	return &Unwrapper{width: width, period: int64(1) << uint(width)}, nil
}

// WrapWidth reports the configured bit width.
func (u *Unwrapper) WrapWidth() int { return u.width }

// Period reports 1<<width.
func (u *Unwrapper) Period() int64 { return u.period }

// Wraps reports how many forward wrap boundaries have been crossed.
func (u *Unwrapper) Wraps() int { return u.wraps }

// Unwrap extends a raw timestamp. The fold is raw mod period; the epoch is
// chosen so the result is the closest candidate to the previous unwrapped
// value. If that candidate moves forward past the next period boundary the
// unwrapper records a wrap.
func (u *Unwrapper) Unwrap(raw int64) int64 {
	folded := raw % u.period
	if folded < 0 {
		folded += u.period
	}
	if !u.seen {
		u.seen = true
		u.epoch = raw - folded
		u.last = u.epoch + folded
		return u.last
	}
	// The nearest epoch is the one that puts the value closest to last.
	bestEpoch := u.epoch
	best := u.epoch + folded
	if d := abs64(best - u.last); d > u.period/2 {
		for _, candEpoch := range []int64{u.epoch - u.period, u.epoch + u.period} {
			cand := candEpoch + folded
			if d2 := abs64(cand - u.last); d2 < d {
				bestEpoch, best, d = candEpoch, cand, d2
			}
		}
	}
	if bestEpoch > u.epoch && best > u.last {
		u.wraps++
	}
	u.epoch = bestEpoch
	u.last = best
	return best
}

// Reset clears the unwrap state.
func (u *Unwrapper) Reset() {
	u.last, u.epoch, u.seen, u.wraps = 0, 0, false, 0
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
