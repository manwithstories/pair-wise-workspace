// Package clock defines the single nanosecond time base shared by every
// pipeline module. RTP timestamps (32-bit, clock-rate dependent) are mapped to
// an int64 nanosecond axis that survives 32-bit and 16-bit wraparound.
//
// Anomalous timestamps are reported, never silently rewritten into a
// continuous timeline: callers decide whether to skip or fail the packet.
package clock

import (
	"mediapipeline/internal/mediaerr"
)

// nsPerSecond is the nanosecond count of one second.
const nsPerSecond int64 = 1_000_000_000

// Timeline converts per-track RTP timestamps into a global nanosecond axis.
// One Timeline instance is safe for sequential (single-goroutine) use; the
// pipeline gives each track its own instance.
type Timeline struct {
	clockRate uint32
	// baseRTP is the first RTP timestamp observed on the track.
	baseRTP uint32
	// baseNS is the nanosecond position assigned to baseRTP.
	baseNS int64
	// extBase is the 64-bit extension of baseRTP after wrap expansion.
	extBase uint64
	// lastExt is the extended value of the previous packet.
	lastExt uint64
	// prevRTP is the raw RTP timestamp of the previous packet.
	prevRTP uint32
	started bool
	// maxJumpNS bounds a plausible forward jump; beyond it the packet is
	// flagged as anomalous instead of being trusted blindly.
	maxJumpNS int64
}

// NewTimeline builds a timeline for an RTP clock rate (e.g. 48000 audio).
// maxJumpNS sets the anomaly threshold; pass 0 for the default of 5 s.
func NewTimeline(clockRate uint32, maxJumpNS int64) (*Timeline, error) {
	if clockRate == 0 {
		return nil, mediaerr.New(mediaerr.StageClock, mediaerr.KindInvalidInput,
			"RTP clock rate must be non-zero")
	}
	if maxJumpNS <= 0 {
		maxJumpNS = 5 * nsPerSecond
	}
	return &Timeline{clockRate: clockRate, maxJumpNS: maxJumpNS}, nil
}

// Extend expands a 32-bit RTP timestamp to 64 bits relative to a reference
// extended value, handling one forward or backward wrap. It is also used for
// 16-bit sequence numbers when width16 is true.
func Extend(ref uint64, v uint32, width16 bool) uint64 {
	mod := uint64(1) << 32
	if width16 {
		mod = uint64(1) << 16
	}
	delta := int64(v) - int64(ref%mod)
	if delta > int64(mod/2) {
		delta -= int64(mod)
	} else if delta < -int64(mod/2) {
		delta += int64(mod)
	}
	return uint64(int64(ref) + delta)
}

// Anchor fixes the timeline origin at the first observed RTP timestamp and
// assigns it to nanosecond position startNS. It is idempotent.
func (t *Timeline) Anchor(firstRTP uint32, startNS int64) {
	if t.started {
		return
	}
	t.started = true
	t.baseRTP = firstRTP
	t.baseNS = startNS
	t.extBase = uint64(firstRTP)
	t.lastExt = uint64(firstRTP)
	t.prevRTP = firstRTP
}

// Started reports whether Anchor has been called.
func (t *Timeline) Started() bool { return t.started }

// ToNS maps an RTP timestamp to the global nanosecond axis. Before Anchor the
// raw conversion relative to zero is returned. A non-nil error means the
// timestamp is anomalous; the returned nanosecond value is still the literal
// wrap-aware conversion, and callers must not smooth it silently.
func (t *Timeline) ToNS(rtpTS uint32) (int64, *mediaerr.Error) {
	if !t.started {
		return t.literalNS(rtpTS), nil
	}
	ext := Extend(t.lastExt, rtpTS, false)
	ns := t.baseNS + t.ticksToNS(int64(ext-t.extBase))
	var err *mediaerr.Error
	delta := int64(ext - t.lastExt)
	deltaNS := t.ticksToNS(delta)
	if delta < 0 && -deltaNS > t.maxJumpNS {
		err = mediaerr.New(mediaerr.StageClock, mediaerr.KindTimestampJump,
			"RTP timestamp moved backwards beyond threshold").WithTS(rtpTS)
	} else if delta > 0 && deltaNS > t.maxJumpNS {
		err = mediaerr.New(mediaerr.StageClock, mediaerr.KindTimestampJump,
			"RTP timestamp jumped forward beyond threshold").WithTS(rtpTS)
	}
	t.lastExt = ext
	t.prevRTP = rtpTS
	return ns, err
}

// literalNS converts ticks relative to a zero origin.
func (t *Timeline) literalNS(rtpTS uint32) int64 {
	return t.ticksToNS(int64(rtpTS))
}

// ticksToNS converts clock-rate ticks to nanoseconds with rounded division.
func (t *Timeline) ticksToNS(ticks int64) int64 {
	num := ticks * nsPerSecond
	rate := int64(t.clockRate)
	if num >= 0 {
		return (num + rate/2) / rate
	}
	return -((-num + rate/2) / rate)
}

// NSToTicks maps a nanosecond position back to RTP clock ticks.
func (t *Timeline) NSToTicks(ns int64) int64 {
	rate := int64(t.clockRate)
	num := ns * rate
	if num >= 0 {
		return (num + nsPerSecond/2) / nsPerSecond
	}
	return -((-num + nsPerSecond/2) / nsPerSecond)
}

// ClockRate returns the RTP clock rate of the timeline.
func (t *Timeline) ClockRate() uint32 { return t.clockRate }

// BaseNS returns the anchored origin in nanoseconds (0 before anchoring).
func (t *Timeline) BaseNS() int64 { return t.baseNS }

// PrevRTP returns the last raw RTP timestamp seen.
func (t *Timeline) PrevRTP() (uint32, bool) { return t.prevRTP, t.started }

// TicksPerNS is a convenience for audio block sizing.
func TicksPerNS(clockRate uint32, ns int64) int64 {
	num := ns * int64(clockRate)
	if num >= 0 {
		return (num + nsPerSecond/2) / nsPerSecond
	}
	return -((-num + nsPerSecond/2) / nsPerSecond)
}
