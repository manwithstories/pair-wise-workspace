package timeline

// SRAnchor is one RTCP sender-report anchor: the RTP timestamp observed at the
// reported NTP instant. NTP time is stored in 64-bit NTP fixed-point units
// (upper 32 seconds, lower 32 fraction) exactly as carried on the wire.
type SRAnchor struct {
	NTPTime uint64 // NTP timestamp from the SR
	RTPTime uint32 // RTP timestamp paired with NTPTime in the SR
}

// Clock maps 32-bit wrapping RTP timestamps onto a monotonic 64-bit media
// timeline (in RTP ticks) and, when at least one sender report is known, onto
// wall-clock NTP time. It is safe for single-goroutine use, which matches the
// pipeline: one clock per rendition, driven from one reorder reader.
type Clock struct {
	rate     uint64
	anchor   *SRAnchor
	baseRTP  uint64 // extended RTP value of the first observed packet
	baseSet  bool
	lastExt  uint64 // most recent extended RTP timestamp
	lastSet  bool
	anchors  int
	jumps    int
}

// NewClock returns a clock for the given RTP clock rate (90000 for video).
func NewClock(rate uint32) *Clock {
	return &Clock{rate: uint64(rate)}
}

// Observe extends a raw 32-bit RTP timestamp to a monotonic 64-bit tick count.
// It handles wraparound and rejects implausible clock jumps measured between
// adjacent observations (real inter-packet spacing at 25 fps is 3600 ticks).
// The first call establishes the base. Returned ticks share an arbitrary but
// stable origin (the first observed timestamp), suitable for duration and
// cut-point math.
func (c *Clock) Observe(rtp uint32) (uint64, error) {
	v := uint64(rtp)
	if !c.baseSet {
		c.baseRTP = v
		c.baseSet = true
		c.lastExt = v
		c.lastSet = true
		return 0, nil
	}
	ext := extend32(c.lastExt, v)
	delta := ext - c.lastExt
	if delta > MaxSaneJump {
		c.jumps++
		return 0, NewError(ClassFatal, "timeline.observe",
			"rtp clock jump exceeds safe bound", nil)
	}
	c.lastExt = ext
	return ext - c.baseRTP, nil
}

// SetSR records a sender report. Multiple SRs are accepted; the newest one
// re-anchors wall-clock conversion and lets us tolerate clock drift.
func (c *Clock) SetSR(a SRAnchor) error {
	if c.baseSet && c.lastSet {
		// A sender report ahead of the newest media packet is normal (the SR
		// arrives before the packets it describes are drained). Only a report
		// anchored far in the past indicates a corrupt or replayed report.
		if d := delta32(uint32(c.lastExt), a.RTPTime); d < 0 && uint64(-d) > MaxSaneJump {
			return NewError(ClassCorrupt, "timeline.sr",
				"sender report timestamp is far in the past", nil)
		}
	}
	c.anchor = &a
	c.anchors++
	return nil
}

// delta32 returns cur-prev interpreted as a signed 32-bit distance, so a
// genuinely old timestamp (large positive unsigned delta) reads negative.
func delta32(prev, cur uint32) int64 {
	return int64(int32(cur - prev))
}

// ToNTP converts an extended tick (as returned by Observe) to a 64-bit NTP
// timestamp using the most recent sender report. The media-to-NTP conversion is
// done as exact rational arithmetic (whole seconds plus a remainder) so no
// integer-scale drift accumulates over a long program.
func (c *Clock) ToNTP(tick uint64) (uint64, bool) {
	if c.anchor == nil || !c.baseSet {
		return 0, false
	}
	anchorExt := extend32(c.lastExt, uint64(c.anchor.RTPTime))
	anchorTick := anchorExt - c.baseRTP
	if tick >= anchorTick {
		return c.anchor.NTPTime + c.ticksToNTP(tick-anchorTick), true
	}
	back := c.ticksToNTP(anchorTick - tick)
	if back > c.anchor.NTPTime {
		return 0, false
	}
	return c.anchor.NTPTime - back, true
}

// ticksToNTP converts a positive tick delta into NTP fixed-point units without
// overflow: split into whole seconds and a sub-second remainder.
func (c *Clock) ticksToNTP(deltaTicks uint64) uint64 {
	whole := deltaTicks / c.rate
	rem := deltaTicks % c.rate
	return (whole << 32) + (rem<<32)/c.rate
}

// Seconds converts an extended tick offset to seconds on the media timeline.
func (c *Clock) Seconds(tick uint64) float64 {
	return float64(tick) / float64(c.rate)
}

// Anchors reports how many sender reports have been accepted.
func (c *Clock) Anchors() int { return c.anchors }

// Jumps reports how many observations were rejected as clock jumps.
func (c *Clock) Jumps() int { return c.jumps }

// extend32 lifts a 32-bit value onto the 64-bit cycle implied by prev (which is
// already an extended value). It implements RFC 3550 Appendix A.1's modular
// distance: the signed-equivalent 32-bit delta (cur-prev) is added to prev,
// selecting the cur instance nearest to prev. Correct whenever the true gap is
// under 2^31.
func extend32(prev, cur uint64) uint64 {
	const mod = WrapMod
	delta := (cur - prev) & (mod - 1) // (cur - prev) mod 2^32
	if delta >= mod/2 {
		delta -= mod // interpret as a negative distance
	}
	return prev + delta
}
