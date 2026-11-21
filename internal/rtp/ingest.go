// Package rtp ingests raw RTP/RTCP datagrams and produces an ordered,
// deduplicated packet stream with traceable loss intervals.
package rtp

import (
	"errors"
	"fmt"

	"github.com/pion/rtp"
)

// Kind distinguishes the two datagram types carried on the RTP local port.
type Kind int

const (
	KindRTP Kind = iota + 1
	KindRTCP
)

// Datagram is one parsed UDP payload plus its 0-based arrival index.
type Datagram struct {
	Arrival int
	Kind    Kind
	Packet  *rtp.Packet // set for KindRTP
	Reports []RTCPReport
	Raw     []byte
}

// RTCPReport is the calibration-relevant subset of an RTCP sender report.
type RTCPReport struct {
	SSRC         uint32
	NTPTime      uint64
	RTPTimestamp uint32
}

// ErrMalformed is returned for datagrams that cannot be parsed safely. The
// pipeline counts these as unrecoverable input rather than guessing.
var ErrMalformed = errors.New("rtp: malformed datagram")

// ParseDatagram inspects the first bytes and dispatches RTP (RFC 3550 §5.1)
// or RTCP compound packets (§6). RTCP SRs are parsed locally because the
// pinned pion/rtp version ships no rtcp subpackage.
func ParseDatagram(arrival int, buf []byte) (Datagram, error) {
	if len(buf) < 2 {
		return Datagram{Arrival: arrival}, fmt.Errorf("%w: %d bytes", ErrMalformed, len(buf))
	}
	if buf[0]&0xC0 != 0x80 { // RTP version 2 in the top two bits
		return Datagram{Arrival: arrival}, fmt.Errorf("%w: RTP version != 2", ErrMalformed)
	}
	// RFC 5761 demultiplexing: RTCP is restricted to PT 192..223 on this
	// capture (the synthetic generator only emits sender reports, PT 200).
	// RTP uses dynamic PT 96, whose marker-set byte (224) stays above 223,
	// so comparing the full second byte keeps the two classes disjoint.
	pt := int(buf[1])
	if 192 <= pt && pt <= 223 {
		if pt != 200 {
			return Datagram{Arrival: arrival}, fmt.Errorf("%w: unsupported RTCP PT=%d", ErrMalformed, pt)
		}
		reps, err := parseSenderReports(buf)
		if err != nil {
			return Datagram{Arrival: arrival}, err
		}
		return Datagram{Arrival: arrival, Kind: KindRTCP, Reports: reps, Raw: append([]byte(nil), buf...)}, nil
	}
	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(buf); err != nil {
		return Datagram{Arrival: arrival}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return Datagram{Arrival: arrival, Kind: KindRTP, Packet: pkt, Raw: append([]byte(nil), buf...)}, nil
}

// parseSenderReports walks a compound RTCP packet containing SR blocks.
// Block layout per RFC 3550 §6.4.1: header 8 bytes, SR info 20 bytes,
// then reception report blocks (6*RC bytes) before the next block.
func parseSenderReports(buf []byte) ([]RTCPReport, error) {
	var out []RTCPReport
	for pos := 0; pos+8 <= len(buf); {
		if buf[pos]&0xC0 != 0x80 {
			return nil, fmt.Errorf("%w: bad RTCP version at offset %d", ErrMalformed, pos)
		}
		pcount := int(buf[pos] & 0x1F)
		pt := buf[pos+1]
		length := int(uint16(buf[pos+2])<<8 | uint16(buf[pos+3]))
		blockBytes := (length + 1) * 4
		if blockBytes <= 0 || pos+blockBytes > len(buf) {
			return nil, fmt.Errorf("%w: RTCP block length overruns buffer", ErrMalformed)
		}
		if pt == 200 { // sender report
			if blockBytes < 28 {
				return nil, fmt.Errorf("%w: SR block too short", ErrMalformed)
			}
			b := buf[pos : pos+blockBytes]
			out = append(out, RTCPReport{
				SSRC:         u32(b[4:8]),
				NTPTime:      uint64(u32(b[8:12]))<<32 | uint64(u32(b[12:16])),
				RTPTimestamp: u32(b[16:20]),
			})
		}
		pos += blockBytes
		_ = pcount // reception blocks are skipped via the length field
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: compound RTCP without SR", ErrMalformed)
	}
	return out, nil
}

func u32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
