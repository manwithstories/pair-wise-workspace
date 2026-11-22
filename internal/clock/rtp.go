package clock

import (
	"fmt"

	"github.com/pion/rtp"

	"medialine/internal/fault"
)

// RTPTimeline converts the RTP timestamps of a packet stream onto the unified
// microsecond timeline. Sequence numbers are validated so that a dropped or
// reordered packet is reported rather than silently shifting the timeline.
type RTPTimeline struct {
	conv      *Converter
	clockRate uint32
	firstSeq  uint16
	lastSeq   uint16
	haveSeq   bool
	lost      int
}

// NewRTPTimeline constructs a timeline for an RTP clock rate (for example
// 90000 for video, 48000 for Opus).
func NewRTPTimeline(clockRate uint32, startUS int64) (*RTPTimeline, error) {
	conv, err := New(Options{Timescale: uint64(clockRate), WrapBits: 32, StartUS: startUS})
	if err != nil {
		return nil, err
	}
	return &RTPTimeline{conv: conv, clockRate: clockRate}, nil
}

// Push processes one packet, returning its microsecond presentation time.
func (t *RTPTimeline) Push(pkt *rtp.Packet) (int64, error) {
	if pkt == nil {
		return 0, fault.New(fault.StageClock, fault.KindCorruptSample, "nil rtp packet")
	}
	if !t.haveSeq {
		t.haveSeq = true
		t.firstSeq = pkt.SequenceNumber
	} else {
		// Sequence numbers are 16-bit; compare with wrap-aware distance.
		dist := int16(pkt.SequenceNumber - t.lastSeq)
		if dist <= 0 {
			return 0, fault.New(fault.StageClock, fault.KindCorruptSample,
				"rtp sequence went backwards: %d after %d", pkt.SequenceNumber, t.lastSeq)
		}
		if dist > 1 {
			t.lost += int(dist) - 1
		}
	}
	t.lastSeq = pkt.SequenceNumber
	us, err := t.conv.Convert(uint64(pkt.Timestamp))
	if err != nil {
		return 0, err
	}
	return us, nil
}

// Lost reports the number of missing RTP sequence numbers seen so far.
func (t *RTPTimeline) Lost() int { return t.lost }

// String makes the timeline easy to print in diagnostics.
func (t *RTPTimeline) String() string {
	return fmt.Sprintf("rtp clock=%dHz lost=%d", t.clockRate, t.lost)
}
