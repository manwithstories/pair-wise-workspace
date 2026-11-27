// Package transport turns indexed samples into RTP packets and applies a
// bounded jitter buffer, NACK cache and single-packet XOR recovery so the
// pipeline tolerates reordering, duplication and burst loss.
package transport

import (
	"github.com/pion/rtp"

	"mediad/internal/index"
	"mediad/internal/model"
)

// DefaultMTU bounds one RTP packet payload; larger samples are split.
const DefaultMTU = 1400

// Packet extends a pion RTP packet with the unified time base and the
// source sample it belongs to.
type Packet struct {
	RTP        *rtp.Packet
	SampleIdx  uint32
	TrackIdx   int
	PTS        model.Timestamp
	DTS        model.Timestamp
	IsKeyframe bool
}

// Stream is a per-track sequence of media packets with a continuous RTP
// sequence number and a 90 kHz (video) / native (audio) RTP clock.
type Stream struct {
	SSRC        uint32
	PayloadType uint8
	TrackIdx    int
	ClockRate   uint32
	nextSeq     uint16
	// Fill produces the media payload bytes for a sample. A nil Fill uses a
	// deterministic pattern so XOR recovery remains verifiable in tests.
	Fill func(index.Sample) []byte
}

// NewStream constructs a stream with a deterministic SSRC derived from the
// track index so repeated runs produce identical packets.
func NewStream(trackIdx int, clockRate uint32) *Stream {
	return &Stream{
		SSRC:        0x4D454400 ^ uint32(trackIdx), // 'MED\0'
		PayloadType: 96 + uint8(trackIdx),
		TrackIdx:    trackIdx,
		ClockRate:   clockRate,
	}
}

// defaultFill returns a deterministic, position-dependent payload so two
// packets differ and XOR reconstruction is non-trivial.
func defaultFill(sample index.Sample) []byte {
	b := make([]byte, sample.Size)
	for i := range b {
		b[i] = byte(uint32(sample.Index)*31 + uint32(i)*7 + 0xA5)
	}
	return b
}

// rtpTimestamp maps a unified nanosecond PTS onto the stream RTP clock.
func (s *Stream) rtpTimestamp(pts model.Timestamp) uint32 {
	units := uint64(pts) * uint64(s.ClockRate) / uint64(model.Second)
	return uint32(units)
}

// Packetize emits one or more RTP packets for a sample, fragmenting on MTU.
// Every fragment of the same sample shares its RTP timestamp; the final
// fragment sets the marker bit.
func (s *Stream) Packetize(sample index.Sample, trackIdx int) []*Packet {
	fill := s.Fill
	if fill == nil {
		fill = defaultFill
	}
	data := fill(sample)
	maxPayload := DefaultMTU - 12
	if len(data) == 0 {
		data = []byte{0}
	}
	var out []*Packet
	for off := 0; off < len(data); off += maxPayload {
		end := off + maxPayload
		if end > len(data) {
			end = len(data)
		}
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    s.PayloadType,
				SequenceNumber: s.nextSeq,
				Timestamp:      s.rtpTimestamp(sample.PTS),
				SSRC:           s.SSRC,
				Marker:         end == len(data),
			},
			Payload: data[off:end],
		}
		s.nextSeq++
		out = append(out, &Packet{
			RTP:        pkt,
			SampleIdx:  sample.Index,
			TrackIdx:   trackIdx,
			PTS:        sample.PTS,
			DTS:        sample.DTS,
			IsKeyframe: sample.IsKeyframe,
		})
	}
	return out
}

// PacketizeTrack packetizes every sample of a track in DTS order.
func PacketizeTrack(track index.Track) []*Packet {
	clockRate := track.Timescale
	if clockRate == 0 {
		clockRate = 90000
	}
	s := NewStream(0, clockRate)
	out := make([]*Packet, 0, len(track.Samples))
	for _, sm := range track.Samples {
		out = append(out, s.Packetize(sm, 0)...)
	}
	return out
}
