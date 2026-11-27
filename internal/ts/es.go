package ts

import "fmt"

// NALUnitType scans the first Annex-B NAL unit in an H.264 PES payload and
// reports its type. It returns -1 when no start code is present.
func NALUnitType(b []byte) int {
	for i := 0; i+4 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			return int(b[i+3] & 0x1f)
		}
		if i+4 < len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1 {
			return int(b[i+4] & 0x1f)
		}
	}
	return -1
}

// IsIDR reports whether the payload begins with an IDR slice.
func IsIDR(b []byte) bool { return NALUnitType(b) == 5 }

// ADTSHeader inspects the first AAC ADTS frame.
func ADTSHeader(b []byte) (profile, rateIndex, channels int, ok bool) {
	if len(b) < 7 || b[0] != 0xFF || b[1]&0xF6 != 0xF0 {
		return 0, 0, 0, false
	}
	profile = int(b[2]>>6) + 1
	rateIndex = int(b[2] >> 2 & 0x0f)
	channels = int(b[2]&0x01)<<2 | int(b[3]>>6)
	return profile, rateIndex, channels, true
}

// String describes a packet for diagnostics.
func (p *Packet) String() string {
	return fmt.Sprintf("pid=%#04x pusi=%v cc=%d payload=%d pcr=%v", p.PID, p.PUSI, p.CC, len(p.Payload), p.PCRSet)
}
