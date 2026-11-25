package engine

import (
	"errors"
	"sort"

	"github.com/pion/rtp"

	"github.com/fieldcut/fieldcut/internal/base"
)

// RTPProbe reassembles access units over an in-process RTP transport before
// they reach the MP4 writer. Sequence numbers and timestamps are validated;
// disordered packets are reordered, missing packets open a gap.
type RTPProbe struct {
	ssrc       uint32
	nextSeq    uint16
	haveSeq    bool
	baseTS     uint32
	haveTS     bool
	reorderBuf map[uint16]*rtp.Packet
	maxJitter  int
	Losses     int
	Reordered  int
	BadTS      int
}

// NewRTPProbe builds a probe for a fixed SSRC and reorder window.
func NewRTPProbe(ssrc uint32, maxJitter int) *RTPProbe {
	return &RTPProbe{ssrc: ssrc, maxJitter: maxJitter, reorderBuf: map[uint16]*rtp.Packet{}}
}

// Push accepts one packet. It returns packets in emission order; a returned
// fault (always recoverable) reports the rendition plus the shared-clock PTS
// supplied by the caller.
func (p *RTPProbe) Push(pkt *rtp.Packet, loc base.Locator) ([]*rtp.Packet, []*base.Fault) {
	var faults []*base.Fault
	if pkt.SSRC != p.ssrc {
		return nil, []*base.Fault{base.NewFault(base.FaultRTPLoss, loc, true,
			"ssrc mismatch: got %d want %d", pkt.SSRC, p.ssrc)}
	}
	if p.haveTS && int32(pkt.Timestamp-p.baseTS) < 0 {
		p.BadTS++
		faults = append(faults, base.NewFault(base.FaultRTPOrder, loc, true,
			"timestamp %d precedes base %d", pkt.Timestamp, p.baseTS))
	}
	if !p.haveTS {
		p.baseTS, p.haveTS = pkt.Timestamp, true
	}

	if !p.haveSeq {
		p.nextSeq, p.haveSeq = pkt.SequenceNumber, true
	}
	dist := int(int16(pkt.SequenceNumber - p.nextSeq))
	switch {
	case dist < 0:
		// Late duplicate of an already-emitted sequence: drop, count loss.
		p.Losses++
		faults = append(faults, base.NewFault(base.FaultRTPLoss, loc, true,
			"late/duplicate sequence %d", pkt.SequenceNumber))
		return nil, faults
	case dist > p.maxJitter:
		// Unrecoverably outside the reorder window: declare the gap.
		p.Losses += int(dist)
		faults = append(faults, base.NewFault(base.FaultRTPLoss, loc, true,
			"sequence jump %d -> %d beyond jitter window", p.nextSeq, pkt.SequenceNumber))
		p.nextSeq = pkt.SequenceNumber
	case dist > 0:
		p.reorderBuf[pkt.SequenceNumber] = pkt
	}

	var out []*rtp.Packet
	for {
		pkt2, ok := p.reorderBuf[p.nextSeq]
		if !ok {
			break
		}
		delete(p.reorderBuf, p.nextSeq)
		out = append(out, pkt2)
		p.nextSeq++
	}
	if dist > 0 && dist <= p.maxJitter && len(out) > 0 {
		p.Reordered++
	}
	if dist == 0 {
		out = append(out, pkt)
		p.nextSeq++
	}
	return out, faults
}

// Flush reports any packets stranded when the stream ends as loss.
func (p *RTPProbe) Flush() int {
	if len(p.reorderBuf) == 0 {
		return 0
	}
	keys := make([]int, 0, len(p.reorderBuf))
	for k := range p.reorderBuf {
		keys = append(keys, int(k))
	}
	sort.Ints(keys)
	loss := len(keys)
	p.reorderBuf = map[uint16]*rtp.Packet{}
	p.Losses += loss
	return loss
}

// SendRTP fragments one access unit payload into RTP packets (purely
// in-process) using deterministic sequence numbers and a 90 kHz timestamp.
func SendRTP(ssrc uint32, seq *uint16, ts90k uint32, payloadType uint8, payload []byte, mtu int) []*rtp.Packet {
	if mtu <= 12 {
		mtu = 1400
	}
	var pkts []*rtp.Packet
	for off := 0; off < len(payload); off += mtu {
		end := off + mtu
		if end > len(payload) {
			end = len(payload)
		}
		marker := end == len(payload)
		pkts = append(pkts, &rtp.Packet{
			Header: rtp.Header{
				Version: 2, PayloadType: payloadType,
				SequenceNumber: *seq, Timestamp: ts90k, SSRC: ssrc, Marker: marker,
			},
			Payload: append([]byte(nil), payload[off:end]...),
		})
		*seq++
	}
	if len(pkts) == 0 {
		pkts = append(pkts, &rtp.Packet{
			Header: rtp.Header{Version: 2, PayloadType: payloadType,
				SequenceNumber: *seq, Timestamp: ts90k, SSRC: ssrc, Marker: true},
		})
		*seq++
	}
	return pkts
}

// errRecheck is returned by recovery when a checkpoint hash disagrees with a
// freshly rebuilt segment.
var errRecheck = errors.New("resume recheck mismatch")
