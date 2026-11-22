// Package rtp ingests synthetic RTP streams, reorders them through a
// bounded jitter window, and repairs bounded loss from redundancy and
// neighbouring packets.
package rtp

import (
	"fmt"

	"github.com/pion/rtp"
)

// Packet is one decoded RTP packet on a named track. ExtSeq is the
// wrap-unfolded sequence number assigned by the jitter buffer.
type Packet struct {
	Track     string
	Seq       uint16
	ExtSeq    int64
	Timestamp uint32
	Payload   []byte
	Redundant []byte // copy of the previous packet's payload, if carried
	Arrival   int64  // arrival time in 90 kHz media ticks
	Recovered bool   // set when the packet was synthesised by repair
}

// EncodePayload frames a payload with its redundancy prefix:
// [redLen uint8][redundant bytes][main payload].
func EncodePayload(payload, redundant []byte) []byte {
	n := len(redundant)
	if n > 255 {
		n = 255
	}
	out := make([]byte, 0, 1+n+len(payload))
	out = append(out, byte(n))
	out = append(out, redundant[:n]...)
	out = append(out, payload...)
	return out
}

// Marshal builds a wire-format RTP packet for one track.
func Marshal(seq uint16, ts uint32, ssrc uint32, payload, redundant []byte) ([]byte, error) {
	p := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           ssrc,
		},
		Payload: EncodePayload(payload, redundant),
	}
	return p.Marshal()
}

// Parse decodes one wire-format RTP packet for track, stamping it with
// the given arrival time in media ticks.
func Parse(data []byte, track string, arrival int64) (*Packet, error) {
	var p rtp.Packet
	if err := p.Unmarshal(data); err != nil {
		return nil, fmt.Errorf("rtp: track %q: %w", track, err)
	}
	raw := p.Payload
	redLen := 0
	if len(raw) > 0 {
		redLen = int(raw[0])
		if 1+redLen > len(raw) {
			return nil, fmt.Errorf("rtp: track %q seq %d: redundancy length %d exceeds payload %d",
				track, p.SequenceNumber, redLen, len(raw))
		}
	}
	pkt := &Packet{
		Track:     track,
		Seq:       p.SequenceNumber,
		Timestamp: p.Timestamp,
		Arrival:   arrival,
	}
	if redLen > 0 {
		pkt.Redundant = append([]byte(nil), raw[1:1+redLen]...)
	}
	pkt.Payload = append([]byte(nil), raw[1+redLen:]...)
	return pkt, nil
}
