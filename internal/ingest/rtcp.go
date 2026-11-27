package ingest

import (
	"fmt"

	"github.com/pion/rtcp"

	"mediad/internal/media"
)

// ntpEpochOffset is the seconds between the NTP (1900) and Unix (1970)
// epochs.
const ntpEpochOffset = int64(2208988800)

// NTPTimespecToNanos converts a 64-bit NTP timestamp (32.32 fixed point
// seconds since 1900) to Unix nanoseconds.
func NTPTimespecToNanos(ntp uint64) int64 {
	sec := int64(ntp >> 32)
	frac := int64(ntp & 0xffffffff)
	return (sec-ntpEpochOffset)*media.NanosPerSecond +
		frac*media.NanosPerSecond/(1<<32)
}

// ClockMap maps RTP timestamps of one track onto the unified timeline
// using RTCP Sender Report anchors. The conversion keeps the remainder
// from the source clock so long sessions do not accumulate drift.
type ClockMap struct {
	SSRC      uint32
	Rate      int64
	anchored  bool
	rtpAnchor int64 // extended RTP ticks at the anchor
	ntpAnchor int64 // unified timeline nanos at the anchor
	Jumps     int
}

// ClockSnapshot is the serializable part of ClockMap state.
type ClockSnapshot struct {
	Anchored  bool
	RTPAnchor int64
	NTPAnchor int64
	Jumps     int
}

func (c *ClockMap) Snapshot() ClockSnapshot {
	return ClockSnapshot{c.anchored, c.rtpAnchor, c.ntpAnchor, c.Jumps}
}

func (c *ClockMap) Restore(s ClockSnapshot) {
	c.anchored, c.rtpAnchor, c.ntpAnchor, c.Jumps = s.Anchored, s.RTPAnchor, s.NTPAnchor, s.Jumps
}

// Anchored reports whether at least one Sender Report has been
// observed, so RTP timestamps can be mapped onto the unified timeline.
func (c *ClockMap) Anchored() bool { return c.anchored }

// jumpThresholdFraction is the fraction of a second of disagreement
// between the extrapolated and reported RTP clock that is treated as a
// clock jump rather than jitter.
const jumpThresholdFraction = 2 // 1/2 second

// ObserveSR folds one Sender Report into the mapping. When the report
// disagrees with linear extrapolation from the previous anchor by more
// than half a second of media, a clock-jump fault is returned and the
// mapping is re-anchored.
func (c *ClockMap) ObserveSR(ntp uint64, rtpTS uint32, at int64) *media.Fault {
	ntpN := NTPTimespecToNanos(ntp)
	var f *media.Fault
	if c.anchored && c.Rate > 0 {
		deltaN := ntpN - c.ntpAnchor
		pred := c.rtpAnchor + deltaN*c.Rate/media.NanosPerSecond
		diff := int64(rtpTS) - pred
		thresh := c.Rate / jumpThresholdFraction
		if diff > thresh || diff < -thresh {
			f = &media.Fault{Kind: media.FaultClockJump, Op: "ingest.rtcp",
				Track: c.SSRC, At: at,
				Msg: fmt.Sprintf("sender report off by %d ticks (>%d), re-anchoring", diff, thresh)}
			c.Jumps++
		}
	}
	c.ntpAnchor = ntpN
	c.rtpAnchor = int64(rtpTS)
	c.anchored = true
	return f
}

// ToMediaTime converts an extended RTP timestamp to the unified
// timeline. Before the first Sender Report it falls back to ticks since
// the stream start so ordering stays defined.
func (c *ClockMap) ToMediaTime(extTS int64) media.MediaTime {
	if !c.anchored {
		return media.FromTicks(extTS, c.Rate)
	}
	mt := media.FromTicks(extTS-c.rtpAnchor, c.Rate)
	return media.MediaTime{Nanos: c.ntpAnchor + mt.Nanos, Rem: mt.Rem, Rate: c.Rate}
}

// ParseRTCP decodes one compound RTCP datagram and folds every Sender
// Report into the matching clock map. Unknown SSRCs are skipped.
func ParseRTCP(raw []byte, at int64, maps map[uint32]*ClockMap) ([]*media.Fault, error) {
	pkts, err := rtcp.Unmarshal(raw)
	if err != nil {
		return nil, &media.Fault{Kind: media.FaultParse, Op: "ingest.rtcp",
			At: at, Msg: err.Error()}
	}
	var faults []*media.Fault
	for _, p := range pkts {
		sr, ok := p.(*rtcp.SenderReport)
		if !ok {
			continue
		}
		m := maps[sr.SSRC]
		if m == nil {
			continue
		}
		if f := m.ObserveSR(uint64(sr.NTPTime), sr.RTPTime, at); f != nil {
			faults = append(faults, f)
		}
	}
	return faults, nil
}
