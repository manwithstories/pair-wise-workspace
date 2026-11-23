package demux

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/pion/rtp"
)

// RTP timing sidecars accompany some supplier deliveries (captures of the
// contribution feed). Packets are stored length-prefixed: a 2-byte
// big-endian length followed by the marshalled RTP packet.

// RTPBounds summarizes an RTP timing sidecar.
type RTPBounds struct {
	Packets        int
	FirstTimestamp uint32
	LastTimestamp  uint32
	ClockRateHint  uint32 // samples per second, carried in the SSRC field by convention
}

// ParseRTPSidecar decodes a length-prefixed RTP packet stream and reports
// its timestamp bounds, used to cross-check container timelines.
func ParseRTPSidecar(r io.Reader) (RTPBounds, error) {
	var b RTPBounds
	var hdr [2]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if err == io.EOF {
				return b, nil
			}
			if err == io.ErrUnexpectedEOF {
				return b, fmt.Errorf("rtp sidecar truncated after %d packets", b.Packets)
			}
			return b, err
		}
		buf := make([]byte, binary.BigEndian.Uint16(hdr[:]))
		if _, err := io.ReadFull(r, buf); err != nil {
			return b, fmt.Errorf("rtp sidecar truncated in packet %d", b.Packets)
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf); err != nil {
			return b, fmt.Errorf("rtp packet %d: %w", b.Packets, err)
		}
		if b.Packets == 0 {
			b.FirstTimestamp = pkt.Timestamp
			b.ClockRateHint = pkt.SSRC
		}
		b.LastTimestamp = pkt.Timestamp
		b.Packets++
	}
}

// WriteRTPSidecar marshals packets in the length-prefixed sidecar format.
// Used by fixture generation and by round-trip tests.
func WriteRTPSidecar(w io.Writer, packets []*rtp.Packet) error {
	for _, p := range packets {
		raw, err := p.Marshal()
		if err != nil {
			return err
		}
		var hdr [2]byte
		binary.BigEndian.PutUint16(hdr[:], uint16(len(raw)))
		if _, err := w.Write(hdr[:]); err != nil {
			return err
		}
		if _, err := w.Write(raw); err != nil {
			return err
		}
	}
	return nil
}
