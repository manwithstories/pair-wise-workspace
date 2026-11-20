// Package timeline maps per-track RTP timestamps onto a unified
// nanosecond time base, handling 32-bit wraparound, backward jumps
// and clock drift against RTCP Sender Reports.
package timeline

import "fmt"

// EventKind classifies anomalies detected during mapping.
type EventKind int

const (
	EvNone EventKind = iota
	EvWrap
	EvBackward
	EvDrift
)

// Event describes one clock anomaly; DeviationNs is the magnitude.
type Event struct {
	Kind        EventKind
	DeviationNs int64
	Detail      string
}

// Clock maps one track's 32-bit RTP timestamps to nanoseconds.
type Clock struct {
	Rate uint32

	init    bool
	lastExt uint64 // extended timestamp of previous packet
	adjust  int64  // accumulated correction (in RTP units) keeping ns monotonic

	// Anchors bind extended RTP time to the absolute base.
	// The first anchor is estimated from the first packet until a
	// Sender Report provides an authoritative one.
	anchorRTP int64 // extended RTP units
	anchorNs  int64 // nanoseconds corresponding to anchorRTP
	Estimated bool

	prevSR *srAnchor
	driftPPM uint32
}

type srAnchor struct {
	rtpExt int64
	ntpNs  int64
}

func NewClock(rate uint32, driftThresholdPPM uint32) *Clock {
	return &Clock{Rate: rate, Estimated: true, driftPPM: driftThresholdPPM}
}

// Map converts an RTP timestamp to the unified nanosecond base.
// It returns the mapped time, the corrected extended timestamp
// (monotonic even across backward jumps), and any anomaly.
func (c *Clock) Map(ts uint32) (int64, int64, *Event) {
	if !c.init {
		c.init = true
		c.lastExt = uint64(ts)
		if c.Estimated {
			// No Sender Report yet: estimate the anchor from the
			// first packet. An existing SR anchor is kept.
			c.anchorRTP = int64(c.lastExt)
			c.anchorNs = 0
			return 0, int64(c.lastExt), nil
		}
		return c.toNs(c.lastExt), int64(c.lastExt), nil
	}
	ext := (c.lastExt &^ 0xFFFFFFFF) | uint64(ts)
	switch d := int64(ext) - int64(c.lastExt); {
	case d > 0x80000000 && ext >= 1<<32:
		// Jump forward by more than half the range: this packet
		// belongs to the previous cycle (backward step).
		trueExt := ext - (1 << 32)
		dev := c.ticksToNs(int64(c.lastExt - trueExt))
		c.adjust += int64(c.lastExt) - int64(trueExt)
		c.lastExt = trueExt
		return c.toNs(trueExt), int64(trueExt) + c.adjust, &Event{Kind: EvBackward, DeviationNs: dev,
			Detail: "timestamp moved backward across cycle boundary"}
	case d < -0x80000000:
		// 32-bit wraparound: continue in the next cycle.
		ext += 1 << 32
		c.lastExt = ext
		return c.toNs(ext), int64(ext) + c.adjust, &Event{Kind: EvWrap, DeviationNs: 0,
			Detail: "32-bit timestamp wraparound"}
	case d < 0:
		// Plain backward jump: report, then correct so the
		// mapped time stays monotonic.
		dev := c.ticksToNs(-d)
		c.adjust += -d
		c.lastExt = ext
		return c.toNs(ext), int64(ext) + c.adjust, &Event{Kind: EvBackward, DeviationNs: dev,
			Detail: "timestamp moved backward"}
	default:
		c.lastExt = ext
		return c.toNs(ext), int64(ext) + c.adjust, nil
	}
}

// AddSenderReport anchors RTP time to absolute NTP time and checks
// the drift between consecutive reports against the nominal rate.
func (c *Clock) AddSenderReport(rtpTS uint32, ntpNanos int64, curExt uint64) *Event {
	ext := int64((curExt &^ 0xFFFFFFFF) | uint64(rtpTS))
	if int64(curExt)-ext > 0x80000000 {
		ext += 1 << 32
	} else if ext-int64(curExt) > 0x80000000 {
		ext -= 1 << 32
	}
	a := srAnchor{rtpExt: ext, ntpNs: ntpNanos}
	var ev *Event
	if c.prevSR != nil && c.driftPPM > 0 {
		dRTP := a.rtpNsDelta(c.prevSR, c.Rate)
		dNTP := a.ntpNs - c.prevSR.ntpNs
		if dNTP > 0 && dRTP > 0 {
			ppm := (dRTP - dNTP) * 1_000_000 / dNTP
			if ppm < 0 {
				ppm = -ppm
			}
			if uint32(ppm) > c.driftPPM {
				dev := dRTP - dNTP
				if dev < 0 {
					dev = -dev
				}
				ev = &Event{Kind: EvDrift, DeviationNs: dev,
					Detail: fmt.Sprintf("clock drift %d ppm exceeds %d ppm", ppm, c.driftPPM)}
			}
		}
	}
	c.prevSR = &a
	// Re-anchor on the authoritative report.
	c.anchorRTP = a.rtpExt + c.adjust
	c.anchorNs = a.ntpNs
	c.Estimated = false
	return ev
}

func (a *srAnchor) rtpNsDelta(prev *srAnchor, rate uint32) int64 {
	return (a.rtpExt - prev.rtpExt) * 1_000_000_000 / int64(rate)
}

func (c *Clock) toNs(ext uint64) int64 {
	adj := int64(ext) + c.adjust
	return c.anchorNs + (adj-c.anchorRTP)*1_000_000_000/int64(c.Rate)
}

func (c *Clock) ticksToNs(ticks int64) int64 {
	return ticks * 1_000_000_000 / int64(c.Rate)
}

// Export returns the clock state for checkpointing.
func (c *Clock) Export() (init bool, lastExt uint64, adjust, anchorRTP, anchorNs int64, estimated bool) {
	return c.init, c.lastExt, c.adjust, c.anchorRTP, c.anchorNs, c.Estimated
}

// Import restores a previously exported clock state.
func (c *Clock) Import(init bool, lastExt uint64, adjust, anchorRTP, anchorNs int64, estimated bool) {
	c.init, c.lastExt, c.adjust = init, lastExt, adjust
	c.anchorRTP, c.anchorNs, c.Estimated = anchorRTP, anchorNs, estimated
}
