// Frame assembly: in-order packet accumulation, marker/timestamp flushing and
// payload digesting, kept separate from the reordering/gap state machine.
package rtp

import (
	"crypto/sha256"

	pionrtp "github.com/pion/rtp"
)

// deliver appends one in-order packet to the current frame, flushing on a
// marker bit or a timestamp change.
func (d *Depacketizer) deliver(p *pionrtp.Packet) []Frame {
	var out []Frame
	if d.curPackets > 0 && p.Timestamp != d.curTimestamp {
		out = append(out, d.flushFrame())
	}
	if d.curPackets == 0 {
		d.curTimestamp = p.Timestamp
		d.curFirst = p.SequenceNumber
		d.curPayload = d.curPayload[:0]
	}
	d.curLast = p.SequenceNumber
	d.curPackets++
	d.curPayload = append(d.curPayload, p.Payload...)
	if p.Marker {
		out = append(out, d.flushFrame())
	}
	return out
}

func (d *Depacketizer) flushFrame() Frame {
	sum := sha256.Sum256(d.curPayload)
	payload := make([]byte, len(d.curPayload))
	copy(payload, d.curPayload)
	fr := Frame{
		Index:       d.frameIndex,
		FirstSeq:    d.curFirst,
		LastSeq:     d.curLast,
		Timestamp:   d.curTimestamp,
		Bytes:       len(payload),
		SHA256:      hexSum(sum[:]),
		PacketCount: d.curPackets,
		Payload:     payload,
	}
	d.frameIndex++
	d.curPackets = 0
	d.curPayload = d.curPayload[:0]
	d.stats.Frames++
	d.stats.Bytes += int64(fr.Bytes)
	return fr
}

func hexSum(b []byte) string {
	const hexChars = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexChars[v>>4]
		out[i*2+1] = hexChars[v&0x0f]
	}
	return string(out)
}
