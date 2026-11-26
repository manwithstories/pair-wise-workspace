package clock

import (
	"math"

	"pairflow/internal/model"
)

// Uint32Mod is the RTP timestamp modulus.
const Uint32Mod uint64 = 1 << 32

// DefaultMaxForwardDelta bounds a plausible continuous timestamp step.
// At 90 kHz this is roughly 477 seconds, far above any normal frame interval.
const DefaultMaxForwardDelta = Uint32Mod / 2

// Unwrapper reconstructs the 64-bit RTP timestamp while distinguishing a
// modulo wrap from a device restart/reset.
type Unwrapper struct {
	last       uint32
	ext        uint64
	cycles     uint64
	started    bool
	restarts   int
	maxForward int64
}

// NewUnwrapper creates an RTP timestamp unwrapper.
func NewUnwrapper() *Unwrapper {
	return &Unwrapper{maxForward: int64(DefaultMaxForwardDelta)}
}

// Restarts reports detected device resets.
func (u *Unwrapper) Restarts() int { return u.restarts }

// Cycles reports the number of complete 32-bit wrap cycles.
func (u *Unwrapper) Cycles() uint64 { return u.cycles }

// Extended returns the latest reconstructed timestamp.
func (u *Unwrapper) Extended() uint64 { return u.ext }

// Observe consumes one monotonic source timestamp. The returned diagnostic is
// non-nil for a restart; ordinary wraps are counted without an error.
func (u *Unwrapper) Observe(ts uint32) (uint64, *model.MediaError) {
	if !u.started {
		u.started = true
		u.last = ts
		u.ext = uint64(ts)
		return u.ext, nil
	}

	// Modular forward distance from last to ts.
	distance := int64(ts) - int64(u.last)
	if distance < 0 {
		distance += int64(Uint32Mod)
	}

	var event *model.MediaError
	switch {
	case ts == u.last:
		// Duplicate timestamp; extended value is unchanged.
	case distance <= u.maxForward:
		if ts < u.last {
			u.cycles++
		}
		u.ext += uint64(distance)
	default:
		// A low timestamp whose modular forward distance is implausibly large
		// is a device restart rather than a wrap. Move to a fresh epoch above
		// every previously observed value so track time never goes backwards.
		u.restarts++
		epoch := u.ext/Uint32Mod + 1
		u.cycles = epoch
		u.ext = epoch*Uint32Mod + uint64(ts)
		event = &model.MediaError{
			Code:              model.CodeClockRestart,
			TrackID:           "",
			ExtendedTimestamp: u.ext,
			Message:           "RTP timestamp reset indicates device restart",
			Recoverable:       true,
		}
	}
	u.last = ts
	return u.ext, event
}

// UnwrapSequence applies Observe to a complete timestamp sequence.
func UnwrapSequence(timestamps []uint32) ([]uint64, []*model.MediaError) {
	u := NewUnwrapper()
	out := make([]uint64, 0, len(timestamps))
	var events []*model.MediaError
	for _, ts := range timestamps {
		ext, event := u.Observe(ts)
		out = append(out, ext)
		if event != nil {
			events = append(events, event)
		}
	}
	return out, events
}

// SafeInt64NS clamps a nanosecond value to the signed range used by records.
func SafeInt64NS(v float64) int64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	if v < math.MinInt64 {
		return math.MinInt64
	}
	return int64(math.Round(v))
}
