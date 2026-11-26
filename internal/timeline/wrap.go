package timeline

import (
	"fmt"
	"math"
)

// Unwrapper expands per-track timestamps that wrap at a fixed modulus (e.g.
// 32-bit RTP sequence numbers or 33-bit MPEG-TS PCR). State is kept per track:
// each track owns an independent Unwrapper because their phases differ.
type Unwrapper struct {
	modulus   int64 // exclusive wrap boundary, > 0
	last      int64 // last expanded value
	prev      int64 // previous raw value in [0, modulus)
	havePrev  bool
	wraps     int
	backwards int
}

// NewUnwrapper requires a positive modulus; 2^32 for RTP, 2^33 for PCR.
func NewUnwrapper(modulus int64) (*Unwrapper, error) {
	if modulus <= 1 {
		return nil, fmt.Errorf("%w: unwrap modulus must be > 1, got %d", ErrInvalidTimebase, modulus)
	}
	return &Unwrapper{modulus: modulus, last: math.MinInt64, prev: math.MinInt64}, nil
}

// Expand feeds the next raw in-range timestamp and returns its monotonic
// expanded form. A jump backwards smaller than half the modulus is treated as
// a wrap (a carry is added); larger backwards jumps are reported as structured
// errors so callers can record the discontinuity instead of hiding it.
func (u *Unwrapper) Expand(track uint32, raw int64, tb Rational) (int64, error) {
	if raw < 0 || raw >= u.modulus {
		return 0, TrackError("timeline.unwrap", track, raw, tb, false,
			fmt.Errorf("%w: raw %d outside modulus %d", ErrOverflow, raw, u.modulus))
	}
	if !u.havePrev {
		u.havePrev = true
		u.prev, u.last = raw, raw
		return raw, nil
	}

	delta := raw - u.prev
	half := u.modulus / 2
	switch {
	case delta == 0:
		// Repeated timestamp (duplicate packet); keep the same expanded value.
		return u.last, nil
	case delta > 0:
		if delta > half {
			// A huge forward raw jump means the counter had already wrapped:
			// the true delta is delta - modulus (e.g. 65535 -> 1).
			expanded := u.last + (delta - u.modulus)
			u.wraps++
			u.record(raw, expanded)
			return expanded, Wrap("timeline.unwrap", track, raw, tb, 0, true,
				fmt.Errorf("%w: rollover #%d at raw=%d", ErrWrap, u.wraps, raw))
		}
		expanded := u.last + delta
		u.record(raw, expanded)
		return expanded, nil
	default:
		// delta < 0: the raw counter moved backwards.
		if -delta > half {
			// Normal wrap: the counter crossed the modulus boundary, e.g.
			// 65535 -> 0 with modulus 65536 (raw delta -65535, real delta +1).
			if u.last > math.MaxInt64-u.modulus {
				return 0, TrackError("timeline.unwrap", track, raw, tb, false,
					fmt.Errorf("%w: expanded timestamp exceeds int64", ErrOverflow))
			}
			expanded := u.last + u.modulus + delta
			u.wraps++
			u.record(raw, expanded)
			return expanded, Wrap("timeline.unwrap", track, raw, tb, 0, true,
				fmt.Errorf("%w: rollover #%d at raw=%d", ErrWrap, u.wraps, raw))
		}
		// Small backwards move: a genuine discontinuity, surface it explicitly.
		u.backwards++
		expanded := u.last + delta
		u.record(raw, expanded)
		return expanded, TrackError("timeline.unwrap", track, raw, tb, true,
			fmt.Errorf("%w: backwards jump %d (backwards=%d)", ErrWrap, delta, u.backwards))
	}
}

func (u *Unwrapper) record(raw, expanded int64) {
	u.prev, u.last = raw, expanded
}

// Wraps reports how many modulus rollovers have been observed.
func (u *Unwrapper) Wraps() int { return u.wraps }

// Backwards reports large non-wrap backwards discontinuities.
func (u *Unwrapper) Backwards() int { return u.backwards }

// Last returns the most recent expanded timestamp.
func (u *Unwrapper) Last() (int64, bool) {
	if !u.havePrev {
		return 0, false
	}
	return u.last, true
}

// Reset clears all per-track state for reuse on a new segment.
func (u *Unwrapper) Reset() {
	u.last, u.prev, u.havePrev, u.wraps, u.backwards = math.MinInt64, math.MinInt64, false, 0, 0
}
