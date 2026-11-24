package clock

import (
	"fmt"
)

// WrapDelta returns the signed distance from prev to cur treating both as
// 32-bit sequence counters. It is used for RTP sequence numbers and
// timestamps; the result is in (-2^31, 2^31).
func WrapDelta(prev, cur uint32) int32 {
	return int32(cur - prev)
}

// Unwrapper extends a stream of 32-bit timestamps into a monotonically
// increasing 64-bit timeline. A 32-bit wrap contributes a small positive
// delta rather than a large regression.
type Unwrapper struct {
	initialized bool
	last        uint32
	extended    int64
}

// Extend returns the extended timestamp for ts. The first call returns zero.
// A backwards jump of half the counter range or more is rejected because it
// cannot be distinguished from corruption.
func (u *Unwrapper) Extend(ts uint32) (int64, error) {
	if !u.initialized {
		u.initialized = true
		u.last = ts
		u.extended = 0
		return 0, nil
	}
	delta := int64(WrapDelta(u.last, ts))
	// A genuine wrap appears as a small positive delta. A regression of a
	// quarter of the counter range or more cannot be told apart from a
	// corrupt stream, so it is rejected instead of silently sorted away.
	if delta < -(1 << 30) {
		return 0, fmt.Errorf("%w: prev=%08x cur=%08x", ErrBadRollbackTick, u.last, ts)
	}
	next := u.extended + delta
	u.last = ts
	u.extended = next
	return next, nil
}

// Reset forgets the last timestamp.
func (u *Unwrapper) Reset() {
	*u = Unwrapper{}
}

// Base couples a track timescale with an unwrapper and returns unified ticks.
type Base struct {
	Timescale uint32
	unwrap    Unwrapper
}

// NewBase validates the timescale and prepares a converter.
func NewBase(timescale uint32) (*Base, error) {
	if timescale == 0 {
		return nil, ErrTimescale
	}
	return &Base{Timescale: timescale}, nil
}

// Tick converts a 32-bit track timestamp into unified ticks, extending
// wraparound first.
func (b *Base) Tick(ts uint32) (Tick, error) {
	ext, err := b.unwrap.Extend(ts)
	if err != nil {
		return 0, err
	}
	return ScaleToTicks(ext, b.Timescale)
}

// FromInt64 converts a wide track timestamp (as used by MP4 sample tables)
// into unified ticks.
func (b *Base) FromInt64(ts int64) (Tick, error) {
	return ScaleToTicks(ts, b.Timescale)
}

// SoundCheck returns an error when t or d cannot represent a real event.
func SoundCheck(t, d Tick) error {
	if err := ValidateDuration(d); err != nil {
		return err
	}
	if _, err := Add(t, d); err != nil {
		return err
	}
	return nil
}
