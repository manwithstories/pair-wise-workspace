// Package clock defines the single media time base shared by the frame
// manifest, RTP reorder, GOP statistics and corruption ranges.
//
// All media durations inside mediascan are expressed as Tick values on a
// monotonic 64-bit axis. RTP timestamps are 32-bit and may wrap; the
// helpers below convert between the two representations.
package clock

import "errors"

// RTPModulus is the 2^32 modulus of an RTP timestamp (RFC 3550).
const RTPModulus int64 = 1 << 32

// MaxTick caps any accepted media position so that frame-index math can
// never overflow int64. Roughly 32.6 days on a 90 kHz clock.
const MaxTick int64 = (1 << 60) - 1

// ErrOverflow is returned when a time-base conversion would overflow.
var ErrOverflow = errors.New("clock: tick overflow")

// Clock is the unified media time base.
type Clock struct {
	// Rate is the tick frequency in Hz (e.g. 90000 for the RTP video clock).
	Rate int64
	// FPS is the capture frame rate; Rate must be divisible by FPS.
	FPS int64
}

// New validates the time base parameters and returns a Clock.
func New(rate, fps int64) (Clock, error) {
	if rate <= 0 || fps <= 0 {
		return Clock{}, errors.New("clock: rate and fps must be positive")
	}
	if rate%fps != 0 {
		return Clock{}, errors.New("clock: rate must be divisible by fps")
	}
	return Clock{Rate: rate, FPS: fps}, nil
}

// TicksPerFrame is the constant tick distance between two frames.
func (c Clock) TicksPerFrame() int64 { return c.Rate / c.FPS }

// FrameTicks returns the absolute media tick of frame index frameIdx
// (frame 0 starts at tick 0).
func (c Clock) FrameTicks(frameIdx int64) (int64, error) {
	if frameIdx < 0 {
		return 0, errors.New("clock: negative frame index")
	}
	v := frameIdx * c.TicksPerFrame()
	if v < 0 || v > MaxTick {
		return 0, ErrOverflow
	}
	return v, nil
}

// Millis converts a tick offset to milliseconds rounded to nearest.
func (c Clock) Millis(ticks int64) int64 {
	if ticks < 0 {
		return -c.Millis(-ticks)
	}
	return (ticks*1000 + c.Rate/2) / c.Rate
}

// FromMillis converts a millisecond offset to ticks (exact when divisible).
func (c Clock) FromMillis(ms int64) (int64, error) {
	if ms > MaxTick/1000 || ms < -MaxTick/1000 {
		return 0, ErrOverflow
	}
	return ms * c.Rate / 1000, nil
}

// Wrap32 folds a monotonic tick position onto the 32-bit RTP axis.
func Wrap32(ticks int64) uint32 {
	m := ticks % RTPModulus
	if m < 0 {
		m += RTPModulus
	}
	return uint32(m)
}

// Delta32 returns the signed forward distance from a to b on the 32-bit
// RTP axis, interpreted in (-2^31, 2^31]. A positive result means b is
// later than a (a wrapped 32-bit timestamp returns a large positive
// delta); a negative result means b arrived early.
func Delta32(a, b uint32) int64 {
	d := int64(b) - int64(a)
	if d <= -RTPModulus/2 {
		d += RTPModulus
	}
	if d > RTPModulus/2 {
		d -= RTPModulus
	}
	return d
}

// Expand32 lifts a 32-bit RTP timestamp onto the monotonic 64-bit axis,
// choosing the candidate closest to the reference position prev.
func Expand32(prev int64, ts uint32) int64 {
	base := prev &^ (RTPModulus - 1)
	cand := base | int64(ts)
	d := cand - prev
	if d > RTPModulus/2 {
		cand -= RTPModulus
	} else if d < -RTPModulus/2 {
		cand += RTPModulus
	}
	return cand
}
