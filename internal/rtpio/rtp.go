// Package rtpio wraps frame payloads into RTP packets and back. Remote feeds
// arrive as RTP, so the reader must prove that depacketizing a track
// reproduces the exact payload bytes and that packet sequence numbers are
// contiguous.
package rtpio

import (
	"fmt"
	"sort"

	"github.com/pion/rtp"

	"medpipe/internal/model"
)

// PayloadType is the RTP payload type used for opaque medpipe frame bytes.
const PayloadType uint8 = 96

// MaxFragment is the largest payload carried by one packet. It is small on
// purpose so multi-packet frames exercise reassembly.
const MaxFragment = 64

// Packetize splits a frame payload into RTP packets. The frame's raw counter
// is the RTP timestamp, which is the natural way to associate fragments with
// a frame without extending the header; the marker bit is set on the final
// fragment. Sequence numbers are zero-based per frame and renumbered by the
// caller when a track is assembled into one stream.
func Packetize(trackID string, fr *model.Frame) ([]*rtp.Packet, error) {
	if len(fr.Payload) == 0 {
		return nil, model.Corruption("rtpio.Packetize", trackID, fr.Seq, fr.Offset, "empty payload")
	}
	var out []*rtp.Packet
	for off := 0; off < len(fr.Payload); off += MaxFragment {
		end := off + MaxFragment
		if end > len(fr.Payload) {
			end = len(fr.Payload)
		}
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    PayloadType,
				SequenceNumber: uint16(len(out)),
				Timestamp:      uint32(fr.RawTS),
				SSRC:           ssrcFor(trackID),
				Marker:         end == len(fr.Payload),
			},
			Payload: append([]byte(nil), fr.Payload[off:end]...),
		}
		// Marshal and unmarshal once so the returned packets are a faithful
		// wire representation rather than a struct the tests happen to share.
		raw, err := pkt.Marshal()
		if err != nil {
			return nil, model.Wrap(model.KindInvariant, "rtpio.Packetize", err)
		}
		var wire rtp.Packet
		if err := wire.Unmarshal(raw); err != nil {
			return nil, model.Wrap(model.KindInvariant, "rtpio.Packetize", err)
		}
		out = append(out, &wire)
	}
	return out, nil
}

func ssrcFor(trackID string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(trackID); i++ {
		h ^= uint32(trackID[i])
		h *= 16777619
	}
	return h
}

// Reassemble rebuilds payloads from packets, grouping fragments by RTP
// timestamp and ordering them by sequence number. It reports a typed
// corruption error for sequence gaps, inconsistent SSRCs, duplicate
// timestamps or a missing final marker, so a transport operator can see
// exactly where a remote feed broke.
func Reassemble(trackID string, pkts []*rtp.Packet) ([][]byte, error) {
	if len(pkts) == 0 {
		return nil, nil
	}
	type frameParts struct {
		firstSeq uint16
		parts    []*rtp.Packet
	}
	byTS := map[uint32]*frameParts{}
	var order []uint32
	for _, pkt := range pkts {
		if pkt.SSRC != ssrcFor(trackID) {
			return nil, model.Corruption("rtpio.Reassemble", trackID, 0, 0,
				fmt.Sprintf("unexpected SSRC %d", pkt.SSRC))
		}
		fp, ok := byTS[pkt.Timestamp]
		if !ok {
			fp = &frameParts{firstSeq: pkt.SequenceNumber}
			byTS[pkt.Timestamp] = fp
			order = append(order, pkt.Timestamp)
		}
		if pkt.SequenceNumber < fp.firstSeq {
			fp.firstSeq = pkt.SequenceNumber
		}
		fp.parts = append(fp.parts, pkt)
	}
	// Track order follows sequence-number order, which is delivery order.
	sort.SliceStable(order, func(i, j int) bool { return byTS[order[i]].firstSeq < byTS[order[j]].firstSeq })
	out := make([][]byte, 0, len(order))
	var prevTS uint32
	for _, ts := range order {
		fp := byTS[ts]
		sort.Slice(fp.parts, func(i, j int) bool { return fp.parts[i].SequenceNumber < fp.parts[j].SequenceNumber })
		var buf []byte
		markers := 0
		for i, p := range fp.parts {
			if i > 0 && p.SequenceNumber != fp.parts[i-1].SequenceNumber+1 {
				return nil, model.Corruption("rtpio.Reassemble", trackID, uint64(ts), 0,
					fmt.Sprintf("sequence gap %d -> %d", fp.parts[i-1].SequenceNumber, p.SequenceNumber))
			}
			if p.Marker {
				markers++
			}
			buf = append(buf, p.Payload...)
		}
		if markers != 1 {
			return nil, model.Corruption("rtpio.Reassemble", trackID, uint64(ts), 0,
				fmt.Sprintf("%d marker bits, want exactly 1", markers))
		}
		if !fp.parts[len(fp.parts)-1].Marker {
			return nil, model.Corruption("rtpio.Reassemble", trackID, uint64(ts), 0, "missing final marker bit")
		}
		if prevTS == ts {
			return nil, model.Corruption("rtpio.Reassemble", trackID, uint64(ts), 0, "duplicate frame timestamp")
		}
		prevTS = ts
		out = append(out, buf)
	}
	return out, nil
}

// RoundTrip packetizes every frame of every track and checks the reassembled
// payloads byte-for-byte. It returns the total packet count.
func RoundTrip(tracks []model.Track, frames map[string][]model.Frame) (int, error) {
	packets := 0
	for _, tr := range tracks {
		fs := frames[tr.ID]
		var all []*rtp.Packet
		for i := range fs {
			pkts, err := Packetize(tr.ID, &fs[i])
			if err != nil {
				return packets, err
			}
			// Renumber so a track is one contiguous RTP stream.
			for _, p := range pkts {
				p.SequenceNumber = uint16(len(all))
				all = append(all, p)
			}
		}
		got, err := Reassemble(tr.ID, all)
		if err != nil {
			return packets, err
		}
		if len(got) != len(fs) {
			return packets, model.New(model.KindInvariant, "rtpio.RoundTrip",
				fmt.Sprintf("track %s reassembled %d frames, want %d", tr.ID, len(got), len(fs)))
		}
		for i := range got {
			if string(got[i]) != string(fs[i].Payload) {
				return packets, model.New(model.KindInvariant, "rtpio.RoundTrip",
					fmt.Sprintf("track %s frame %d payload changed through RTP", tr.ID, fs[i].Seq))
			}
		}
		packets += len(all)
	}
	return packets, nil
}
