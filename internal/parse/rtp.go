package parse

import (
	"fmt"

	"github.com/pion/rtp"

	"avscan/internal/model"
)

// RTPPacketRecord is one framed packet from a synthetic RTP capture.
type RTPPacketRecord struct {
	// StreamName groups packets belonging to one SSRC/track.
	StreamName string
	// ClockRate converts the RTP timestamp to the nanosecond time base.
	ClockRate uint32
	// Raw is one complete RTP datagram.
	Raw []byte
}

// RTPResult summarizes one stream inspection.
type RTPResult struct {
	StreamName   string
	SSRC         uint32
	ClockRate    uint32
	PacketCount  int
	FirstSeq     uint16
	LastSeq      uint16
	GapCount     int
	// Events are projected onto the unified nanosecond axis relative to the
	// first packet's RTP timestamp.
	Events []model.SampleEvent
	Faults []model.Fault
}

// maxRTPPackets bounds memory when reading an untrusted capture.
const maxRTPPackets = 100_000

// ParseRTPStream inspects a sequence of already-framed RTP packets. Sequence
// numbers are checked modulo 2^16 so a legitimate wrap is not a gap.
func ParseRTPStream(recs []RTPPacketRecord) (*RTPResult, error) {
	if len(recs) == 0 {
		return nil, fmt.Errorf("%w: empty RTP stream", model.ErrInvalidArgs)
	}
	if len(recs) > maxRTPPackets {
		return nil, fmt.Errorf("%w: %d packets > %d",
			model.ErrLimitExceeded, len(recs), maxRTPPackets)
	}
	res := &RTPResult{StreamName: recs[0].StreamName, ClockRate: recs[0].ClockRate}
	var baseTS uint32
	var haveBase bool
	var prevSeq uint16
	var havePrev bool
	for i, rec := range recs {
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(rec.Raw); err != nil {
			res.Faults = append(res.Faults, model.NewByteFault(
				model.FaultRTPBoundary, rec.StreamName,
				int64(i), "packet unmarshal: "+err.Error()))
			continue
		}
		if i == 0 {
			res.SSRC = pkt.SSRC
			res.FirstSeq = pkt.SequenceNumber
		}
		if !haveBase {
			baseTS = pkt.Timestamp
			haveBase = true
		}
		// Timestamp delta is unsigned; wrap is handled by uint32 subtraction.
		tsDelta := pkt.Timestamp - baseTS
		startNS, err := model.TicksToNS(uint64(tsDelta), rec.ClockRate)
		if err != nil {
			return nil, fmt.Errorf("rtp timestamp overflow: %w", err)
		}
		res.Events = append(res.Events, model.SampleEvent{
			Index:    uint32(i),
			StartNS:  startNS,
			Offset:   uint64(i),
			Size:     uint32(len(rec.Raw)),
			IsSync:   pkt.Marker,
			TrackID:  0,
		})
		if havePrev {
			want := prevSeq + 1
			if pkt.SequenceNumber != want {
				missing := pkt.SequenceNumber - want // unsigned gap distance
				res.GapCount += int(missing)
				res.Faults = append(res.Faults, model.Fault{
					Kind:        model.FaultRTPSeqGap,
					Source:      rec.StreamName,
					SampleIndex: int64(i),
					TimeNS:      startNS,
					HasTime:     true,
					Message: fmt.Sprintf("seq jump: got %d want %d (%d missing)",
						pkt.SequenceNumber, want, missing),
				})
			}
		}
		prevSeq = pkt.SequenceNumber
		havePrev = true
		res.LastSeq = pkt.SequenceNumber
		res.PacketCount++
	}
	return res, nil
}

// BuildRTPPacket marshals one RTP datagram for fixture generation.
func BuildRTPPacket(ssrc uint32, seq uint16, ts uint32, payload []byte, marker bool) ([]byte, error) {
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           ssrc,
			Marker:         marker,
		},
		Payload: payload,
	}
	return pkt.Marshal()
}
