// Package rtpnet builds deterministic RTP packets from compressed samples with
// github.com/pion/rtp. Packets are never sent over a network; they are the
// transport-ready representation consumed by the remuxer and checksum stage.
// Sequence numbers and timestamps are derived from the unified timebase so a
// given input always produces identical bytes.
package rtpnet

import (
	"github.com/pion/rtp"

	"mediacore/internal/clock"
	"mediacore/internal/sample"
)

// DefaultMTU is the maximum RTP packet size (including header) before a
// compressed sample is fragmented across multiple packets.
const DefaultMTU = 1200

// rtpFixedHeader is the standard 12-byte RTP fixed header (no CSRC/extensions).
const rtpFixedHeader = 12

// TrackConfig fixes the static RTP parameters for one mapped track.
type TrackConfig struct {
	SSRC        uint32
	PayloadType uint8
	ClockRate   uint32
}

// Packetizer emits a deterministic RTP stream per track.
type Packetizer struct {
	cfg TrackConfig
	seq uint16
	mtu int
}

// NewPacketizer starts a packetizer at sequence number zero.
func NewPacketizer(cfg TrackConfig, mtu int) *Packetizer {
	if mtu < rtpFixedHeader+1 {
		mtu = DefaultMTU
	}
	return &Packetizer{cfg: cfg, mtu: mtu}
}

// Packetize turns one compressed sample into one or more RTP packets. Large
// samples are fragmented with marker set only on the final fragment.
func (p *Packetizer) Packetize(s *sample.Sample) ([]*rtp.Packet, error) {
	ts := uint32(clock.ToTimescale(s.PTS, p.cfg.ClockRate))
	maxPayload := p.mtu - rtpFixedHeader
	data := s.Data
	var out []*rtp.Packet
	if len(data) == 0 {
		data = []byte{} // still emit an empty marker packet
	}
	for offset := 0; ; offset += maxPayload {
		end := offset + maxPayload
		if end > len(data) {
			end = len(data)
		}
		frag := data[offset:end]
		marker := end == len(data)
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    p.cfg.PayloadType,
				SequenceNumber: p.seq,
				Timestamp:      ts,
				SSRC:           p.cfg.SSRC,
				Marker:         marker,
			},
			Payload: append([]byte(nil), frag...),
		}
		p.seq++
		out = append(out, pkt)
		if end == len(data) {
			break
		}
	}
	return out, nil
}

// Encode serializes a packet to its wire bytes via pion/rtp.
func Encode(pkt *rtp.Packet) ([]byte, error) {
	return pkt.Marshal()
}

// Decode parses wire bytes back into a packet, used to verify round-tripping.
func Decode(b []byte) (*rtp.Packet, error) {
	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(b); err != nil {
		return nil, err
	}
	return pkt, nil
}
