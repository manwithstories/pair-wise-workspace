package transport

import (
	"github.com/pion/rtp"

	"mediamix/internal/core"
	"mediamix/internal/timeline"
)

// Wire is the RTP wire-format codec for the ingest path. Packets arrive as
// real RTP datagrams; Wire decodes them into Packet and maps each one back onto
// the same extended sequence number the reorder buffer uses, so the byte path
// and the in-memory path stay consistent.
type Wire struct {
	ext *Extender
}

// NewWire creates a wire codec with an independent sequence extender. It must
// use the same extender instance as the buffers it feeds; read with Extender().
func NewWire() *Wire { return &Wire{ext: NewExtender()} }

// Extender exposes the sequence extender shared with the reorder buffers.
func (w *Wire) Extender() *Extender { return w.ext }

// Encode serializes a Packet as an RTP datagram.
func (w *Wire) Encode(p Packet) ([]byte, error) {
	if p.Track == "" {
		return nil, &core.Error{Code: core.CodeBadPacket, Detail: "cannot encode a packet without a track", Fatal: true}
	}
	rtpPkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Marker:         p.Marker,
			PayloadType:    payloadType(p.Track),
			SequenceNumber: p.Seq,
			Timestamp:      p.Timestamp,
			SSRC:           p.SSRC,
		},
		Payload: p.Payload,
	}
	return rtpPkt.Marshal()
}

// Decode parses an RTP datagram into a Packet for a track and assigns the
// extended sequence number used by the reorder buffer.
func (w *Wire) Decode(track string, datagram []byte, arrival timeline.Micros) (Packet, error) {
	if track == "" {
		return Packet{}, &core.Error{Code: core.CodeBadArgument, Detail: "empty track id", Fatal: true}
	}
	var rtpPkt rtp.Packet
	if err := rtpPkt.Unmarshal(datagram); err != nil {
		return Packet{}, &core.Error{Code: core.CodeBadPacket, Track: track,
			Detail: "malformed RTP datagram: " + err.Error(), Fatal: true}
	}
	if rtpPkt.Version != 2 {
		return Packet{}, &core.Error{Code: core.CodeBadPacket, Track: track,
			Detail: "unsupported RTP version", Fatal: true}
	}
	if len(rtpPkt.Payload) == 0 {
		return Packet{}, core.SeqError(core.CodeBadPacket, track, uint32(rtpPkt.SequenceNumber),
			"RTP packet carries no payload")
	}
	ext, err := w.ext.Extend(track, rtpPkt.SequenceNumber)
	if err != nil {
		return Packet{}, core.StampTrack(err, track)
	}
	return Packet{
		Track:     track,
		SSRC:      rtpPkt.SSRC,
		Seq:       rtpPkt.SequenceNumber,
		Ext:       ext,
		Timestamp: rtpPkt.Timestamp,
		Marker:    rtpPkt.Marker,
		Payload:   append([]byte(nil), rtpPkt.Payload...),
		ArrivalUs: arrival,
	}, nil
}

// payloadType maps a track to a deterministic RTP payload type so a decoded
// datagram can be attributed without an out-of-band map.
func payloadType(track string) uint8 {
	h := uint32(2166136261)
	for i := 0; i < len(track); i++ {
		h ^= uint32(track[i])
		h *= 16777619
	}
	// Dynamic range 96..127.
	return uint8(96 + h%32)
}
