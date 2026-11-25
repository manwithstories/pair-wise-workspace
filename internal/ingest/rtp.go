// Package ingest depacketizes RTP-contained MPEG-TS.
package ingest

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/pion/rtp"

	"github.com/pairwise/mediapack/internal/transport"
)

const (
	// RTPPayloadTS is the dynamic payload type used by generated fixtures.
	RTPPayloadTS uint8 = 33
	// TSPacketsPerRTP is the common 7*188 = 1316 byte MP2T payload.
	TSPacketsPerRTP = 7
)

// RTPPacketizer builds deterministic RTP packets carrying TS.
type RTPPacketizer struct {
	SSRC        uint32
	PayloadType uint8
	Sequence    uint16
	Timestamp   uint32
}

// PacketizeTS chunks a concatenated sequence of 188-byte TS packets.
func (p *RTPPacketizer) PacketizeTS(ts []byte) ([]*rtp.Packet, error) {
	if len(ts)%transport.PacketSize != 0 {
		return nil, fmt.Errorf("TS length %d is not a multiple of 188", len(ts))
	}
	chunk := TSPacketsPerRTP * transport.PacketSize
	if chunk > len(ts) && len(ts) > 0 {
		chunk = len(ts)
	}
	var out []*rtp.Packet
	for start := 0; start < len(ts); start += chunk {
		end := start + chunk
		if end > len(ts) {
			end = len(ts)
		}
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    p.PayloadType,
				SequenceNumber: p.Sequence,
				Timestamp:      p.Timestamp,
				SSRC:           p.SSRC,
				Marker:         start == 0,
			},
			Payload: append([]byte(nil), ts[start:end]...),
		}
		if pkt.Header.PayloadType == 0 {
			pkt.Header.PayloadType = RTPPayloadTS
		}
		out = append(out, pkt)
		p.Sequence++
		// Nine 90 kHz ticks per TS packet is a deterministic fixture clock;
		// PCR/PTS remain authoritative for media duration.
		p.Timestamp += uint32((end - start) / transport.PacketSize * 9)
	}
	return out, nil
}

// MarshalRTP is a convenience wrapper used by fixtures and tests.
func MarshalRTP(pkts []*rtp.Packet) ([][]byte, error) {
	out := make([][]byte, 0, len(pkts))
	for _, pkt := range pkts {
		b, err := pkt.Marshal()
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// RTPStats describes depacketization completeness.
type RTPStats struct {
	RTPPackets int
	TSPackets  int
	Gaps       []uint16
}

// Depacketize parses RTP datagrams and returns their concatenated TS bytes.
// Sequence gaps are reported but not concealed.
func Depacketize(datagrams [][]byte) ([]byte, RTPStats, error) {
	var ts bytes.Buffer
	stats := RTPStats{}
	var lastSeq *uint16

	for i, dg := range datagrams {
		var pkt rtp.Packet
		if err := pkt.Unmarshal(dg); err != nil {
			return nil, stats, fmt.Errorf("rtp packet %d: %w", i, err)
		}
		if pkt.Version != 2 {
			return nil, stats, fmt.Errorf("rtp packet %d: unsupported version %d", i, pkt.Version)
		}
		if len(pkt.Payload)%transport.PacketSize != 0 {
			return nil, stats, fmt.Errorf("rtp packet %d payload %d not multiple of 188", i, len(pkt.Payload))
		}
		if lastSeq != nil {
			want := (*lastSeq + 1)
			if pkt.SequenceNumber != want {
				stats.Gaps = append(stats.Gaps, want)
			}
		}
		s := pkt.SequenceNumber
		lastSeq = &s
		stats.RTPPackets++
		n := len(pkt.Payload) / transport.PacketSize
		stats.TSPackets += n
		ts.Write(pkt.Payload)
	}
	return ts.Bytes(), stats, nil
}

// ParseTSFromRTP depacketizes and parses every contained TS packet.
func ParseTSFromRTP(datagrams [][]byte) ([]transport.Packet, RTPStats, error) {
	raw, stats, err := Depacketize(datagrams)
	if err != nil {
		return nil, stats, err
	}
	packets := make([]transport.Packet, 0, len(raw)/transport.PacketSize)
	for off := 0; off < len(raw); off += transport.PacketSize {
		p, err := transport.ParsePacket(raw[off:off+transport.PacketSize], int64(off))
		if err != nil {
			return nil, stats, err
		}
		packets = append(packets, p)
	}
	return packets, stats, nil
}

// ErrEmptyRTP indicates no datagrams were supplied.
var ErrEmptyRTP = errors.New("no RTP datagrams")
