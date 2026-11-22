// Package rtp validates RTP packet ordering and track/SSRC mapping using
// github.com/pion/rtp. It inspects framing only; it never depacketizes or
// decodes media.
package rtp

import (
	"fmt"

	"github.com/pion/rtp"

	"mediapack/internal/clock"
	"mediapack/internal/fault"
)

// Issue classifies a packet-level anomaly.
type Issue int

const (
	IssueOK Issue = iota
	IssueDuplicate
	IssueGap
	IssueReorder
	IssueSSRCChange
	IssueWrap
)

func (i Issue) String() string {
	switch i {
	case IssueDuplicate:
		return "duplicate_packet"
	case IssueGap:
		return "sequence_gap"
	case IssueReorder:
		return "reordered_packet"
	case IssueSSRCChange:
		return "ssrc_change"
	case IssueWrap:
		return "sequence_wrap"
	default:
		return "ok"
	}
}

// Event records one validation finding.
type Event struct {
	Issue    Issue
	Seq      uint16
	Expected uint16
	Detail   string
}

// Validator incrementally checks a packet stream for one track.
type Validator struct {
	trackID     uint32
	ssrc        uint32
	ssrcSeen    bool
	prevSeq     int64 // unwrapped sequence axis
	havePrev    bool
	seen        map[uint16]bool
	packets     int
	duplicates  int
	gaps        int
	reorders    int
	wraps       int
	ssrcChanges int
}

// NewValidator creates a validator bound to a track id.
func NewValidator(trackID uint32) *Validator {
	return &Validator{trackID: trackID, seen: map[uint16]bool{}}
}

// Check parses and validates one raw RTP packet.
func (v *Validator) Check(raw []byte) (*rtp.Packet, Event, error) {
	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(raw); err != nil {
		return nil, Event{}, fault.At(fault.KindCorruptInput, "rtp", -1,
			"malformed RTP packet: "+err.Error())
	}
	ev := Event{Seq: pkt.SequenceNumber}
	if !v.ssrcSeen {
		v.ssrc, v.ssrcSeen = pkt.SSRC, true
	} else if pkt.SSRC != v.ssrc {
		v.ssrcChanges++
		ev.Issue = IssueSSRCChange
		ev.Detail = fmt.Sprintf("ssrc %08x -> %08x on track %d", v.ssrc, pkt.SSRC, v.trackID)
		v.ssrc = pkt.SSRC
	}

	if v.havePrev {
		ev.Expected = uint16(v.prevSeq + 1)
		unwrapped := clock.Unwrap16(v.prevSeq, pkt.SequenceNumber)
		const mod16 = int64(1) << 16
		switch {
		case unwrapped == v.prevSeq:
			v.duplicates++
			ev.Issue, ev.Detail = IssueDuplicate,
				fmt.Sprintf("seq %d received twice", pkt.SequenceNumber)
		case unwrapped > v.prevSeq+1:
			v.gaps++
			ev.Issue = IssueGap
			ev.Detail = fmt.Sprintf("lost %d packet(s) before seq %d",
				unwrapped-v.prevSeq-1, pkt.SequenceNumber)
		case unwrapped < v.prevSeq:
			v.reorders++
			ev.Issue, ev.Detail = IssueReorder,
				fmt.Sprintf("seq %d arrived after %d", pkt.SequenceNumber, uint16(v.prevSeq))
		}
		if unwrapped/mod16 > v.prevSeq/mod16 {
			v.wraps++
			if ev.Issue == IssueOK {
				ev.Issue = IssueWrap
			}
		}
		v.prevSeq = unwrapped
	} else {
		v.prevSeq = int64(pkt.SequenceNumber)
	}
	v.seen[pkt.SequenceNumber] = true
	v.havePrev = true
	v.packets++
	return pkt, ev, nil
}

// Stats summarizes everything observed so far.
type Stats struct {
	Packets       int `json:"packets"`
	Duplicates    int `json:"duplicates"`
	Gaps          int `json:"gaps"`
	Reorders      int `json:"reorders"`
	SequenceWraps int `json:"sequence_wraps"`
	SSRCChanges   int `json:"ssrc_changes"`
}

// Stats returns the cumulative validation counters.
func (v *Validator) Stats() Stats {
	return Stats{
		Packets: v.packets, Duplicates: v.duplicates, Gaps: v.gaps,
		Reorders: v.reorders, SequenceWraps: v.wraps, SSRCChanges: v.ssrcChanges,
	}
}

// TrackID returns the bound track.
func (v *Validator) TrackID() uint32 { return v.trackID }

// SSRC returns the most recently observed synchronization source.
func (v *Validator) SSRC() (uint32, bool) { return v.ssrc, v.ssrcSeen }
