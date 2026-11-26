package rtp

import (
	"encoding/binary"

	"github.com/archive/tsrestore/internal/timeline"
)

// SenderReport is the subset of an RTCP SR (RFC 3550 section 6.4.1) the
// restoration pipeline consumes.
type SenderReport struct {
	SSRC       uint32
	NTPTime    uint64 // 64-bit NTP fixed-point timestamp
	RTPTime    uint32 // RTP timestamp paired with NTPTime
	PacketCount uint32
}

// IsRTCP reports whether a datagram parses as an RTCP compound packet
// (version 2, payload type in the RTCP range).
func IsRTCP(b []byte) bool {
	if len(b) < 2 || b[0]>>6 != 2 {
		return false
	}
	pt := b[1] & 0x7F
	return pt >= PayloadTypeRTCPMin && pt <= PayloadTypeRTCPMax
}

// ParseSenderReport extracts the first SR block from an RTCP compound packet.
// A compound packet without an SR block is reported as Corrupt so the caller
// can count it as bad RTCP and keep going.
func ParseSenderReport(b []byte) (*SenderReport, error) {
	if !IsRTCP(b) {
		return nil, timeline.NewError(timeline.ClassCorrupt, "rtcp.parse",
			"datagram is not RTCP", nil)
	}
	off := 0
	for off+4 <= len(b) {
		pType := b[off+1]
		rc := int(b[off] & 0x1F)
		// Length field is 32-bit words minus one, including the header.
		words := int(binary.BigEndian.Uint16(b[off+2:off+4])) + 1
		blockEnd := off + 4*words
		if words < 1 || blockEnd > len(b) {
			return nil, timeline.NewError(timeline.ClassCorrupt, "rtcp.parse",
				"RTCP block length runs past datagram", nil)
		}
		if pType == SRType {
			// SR: 8-byte header + 20-byte sender info, then rc report blocks.
			if rc != 0 || words < 7 || blockEnd < off+28 {
				return nil, timeline.NewError(timeline.ClassCorrupt, "rtcp.parse",
					"sender report block too short", nil)
			}
			sr := &SenderReport{
				SSRC:        binary.BigEndian.Uint32(b[off+4 : off+8]),
				NTPTime:     binary.BigEndian.Uint64(b[off+8 : off+16]),
				RTPTime:     binary.BigEndian.Uint32(b[off+16 : off+20]),
				PacketCount: binary.BigEndian.Uint32(b[off+20 : off+24]),
			}
			return sr, nil
		}
		off = blockEnd
	}
	return nil, timeline.NewError(timeline.ClassCorrupt, "rtcp.parse",
		"compound RTCP packet contains no sender report", nil)
}
