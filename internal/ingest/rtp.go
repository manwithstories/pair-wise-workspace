package ingest

import (
	"fmt"
	"sort"

	"github.com/pion/rtp"

	"fieldcut/internal/clock"
)

// RTPPacket is the small RTP surface used by FieldCut.
type RTPPacket struct {
	Sequence  uint16
	Timestamp uint32
	Marker    bool
	Payload   []byte
}

// RTPSample is an RTP packet placed on the 64-bit extended timeline.
type RTPSample struct {
	Sequence          uint16
	ExtendedSequence  uint64
	Timestamp         uint32
	ExtendedTimestamp uint64
	PTS               clock.Ns
	Payload           []byte
	Marker            bool
	MissingBefore     int
}

// UnmarshalRTP parses one RTP packet using pion/rtp.
func UnmarshalRTP(raw []byte) (RTPPacket, error) {
	var p rtp.Packet
	if err := p.Unmarshal(raw); err != nil {
		return RTPPacket{}, &clock.Error{
			Code: clock.CodeInvalidArgument, TrackID: -1,
			Message: "bad RTP packet: " + err.Error(), Wrapping: err,
		}
	}
	return RTPPacket{
		Sequence:  p.SequenceNumber,
		Timestamp: p.Timestamp,
		Marker:    p.Marker,
		Payload:   append([]byte(nil), p.Payload...),
	}, nil
}

// ExtendUint16 unwraps a sequence number relative to prevExtended.
func ExtendUint16(prevExtended uint64, v uint16) uint64 {
	high := prevExtended &^ 0xffff
	candidate := high | uint64(v)
	const half = uint64(1) << 15
	if candidate+half < prevExtended {
		candidate += 1 << 16
	} else if candidate > prevExtended+half {
		candidate -= 1 << 16
	}
	return candidate
}

// ExtendUint32 unwraps an RTP timestamp relative to prevExtended.
func ExtendUint32(prevExtended uint64, v uint32) uint64 {
	high := prevExtended &^ 0xffffffff
	candidate := high | uint64(v)
	const half = uint64(1) << 31
	if candidate+half < prevExtended {
		candidate += 1 << 32
	} else if candidate > prevExtended+half {
		candidate -= 1 << 32
	}
	return candidate
}

type seqEntry struct {
	pkt   RTPPacket
	seq   uint64
	tsExt uint64
}

// ParseRTPPackets unwraps 16/32-bit counters, tolerates reordering and reports
// gaps. Missing packets are reported structurally but do not discard received
// media. Overflow and negative/non-monotonic durations are rejected.
func ParseRTPPackets(trackID int, clockRate uint32, rawPackets [][]byte) ([]RTPSample, []*clock.Error, error) {
	if clockRate == 0 {
		return nil, nil, clock.Errorf(clock.CodeInvalidArgument, trackID, "RTP clock rate must not be zero")
	}
	entries := make([]seqEntry, 0, len(rawPackets))
	var seqPrev, tsPrev uint64
	haveSeq := false
	for i, raw := range rawPackets {
		pkt, err := UnmarshalRTP(raw)
		if err != nil {
			if ce, ok := clock.AsError(err); ok {
				ce.TrackID = trackID
				ce.OriginalValue = fmt.Sprintf("rtp_packet_index=%d", i)
			}
			return nil, nil, err
		}
		if !haveSeq {
			seqPrev = uint64(pkt.Sequence)
			tsPrev = uint64(pkt.Timestamp)
			haveSeq = true
		} else {
			seqPrev = ExtendUint16(seqPrev, pkt.Sequence)
			tsPrev = ExtendUint32(tsPrev, pkt.Timestamp)
		}
		entries = append(entries, seqEntry{pkt: pkt, seq: seqPrev, tsExt: tsPrev})
	}

	sort.SliceStable(entries, func(i, j int) bool { return entries[i].seq < entries[j].seq })

	var reports []*clock.Error
	out := make([]RTPSample, 0, len(entries))
	seen := map[uint64]bool{}
	var lastSeq, lastTS, baseTS uint64
	for _, e := range entries {
		if seen[e.seq] {
			continue // duplicate retransmission
		}
		seen[e.seq] = true
		missing := 0
		if len(out) > 0 {
			if e.seq > lastSeq+1 {
				missing = int(e.seq - lastSeq - 1)
				reports = append(reports, &clock.Error{
					Code: clock.CodeMissingPacket, TrackID: trackID,
					OriginalTimestamp: int64(e.pkt.Timestamp),
					OriginalValue:     fmt.Sprintf("missing=%d after_seq=%d before_seq=%d", missing, lastSeq, e.seq),
					Message:           fmt.Sprintf("%d RTP packet(s) missing", missing),
				})
			}
			if e.tsExt < lastTS {
				reports = append(reports, &clock.Error{
					Code: clock.CodeNegative, TrackID: trackID,
					OriginalTimestamp: int64(e.pkt.Timestamp),
					OriginalValue:     fmt.Sprintf("extended_next=%d extended_prev=%d", e.tsExt, lastTS),
					Message:           "RTP timestamp moves backwards in sequence order",
				})
			}
		} else {
			baseTS = e.tsExt
		}
		lastSeq = e.seq
		lastTS = e.tsExt

		relTS := e.tsExt - baseTS
		if relTS > uint64(^uint64(0)>>1) {
			return nil, reports, &clock.Error{
				Code: clock.CodeOverflow, TrackID: trackID,
				OriginalTimestamp: int64(e.pkt.Timestamp),
				OriginalValue:     fmt.Sprintf("extended_timestamp=%d rate=%d", e.tsExt, clockRate),
				Message:           "relative RTP timestamp cannot fit signed nanoseconds",
			}
		}
		pts, err := clock.TicksToNs(int64(relTS), 1, int64(clockRate))
		if err != nil {
			if ce, ok := clock.AsError(err); ok {
				ce.TrackID = trackID
				ce.OriginalTimestamp = int64(e.pkt.Timestamp)
				ce.OriginalValue = fmt.Sprintf("extended_timestamp=%d", e.tsExt)
			}
			return nil, reports, err
		}
		out = append(out, RTPSample{
			Sequence: e.pkt.Sequence, ExtendedSequence: e.seq,
			Timestamp: e.pkt.Timestamp, ExtendedTimestamp: e.tsExt,
			PTS: pts, Payload: e.pkt.Payload, Marker: e.pkt.Marker,
			MissingBefore: missing,
		})
	}
	return out, reports, nil
}
