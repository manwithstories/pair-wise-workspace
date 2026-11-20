// Package rtp parses and repairs RTP/RTCP packet streams. The reader turns
// raw datagrams into model packets or RTCP Sender Reports; the repairer
// reorders, de-duplicates and detects bounded loss without ever holding
// more than its configured window.
package rtp

import (
	"errors"

	"github.com/pion/rtp"

	"archive/internal/model"
)

// DatagramKind distinguishes the two transport payloads.
type DatagramKind int

const (
	KindUnknown DatagramKind = iota
	KindRTP
	KindRTCP
)

// Datagram is one parsed transport element: either an RTP packet or an
// RTCP Sender Report.
type Datagram struct {
	Kind   DatagramKind
	Packet model.Packet
	Report model.SenderReport
}

// srHeaderLen is the fixed byte length of an RTCP SR block (header +
// sender info) before any reception report blocks.
const srHeaderLen = 28

// ParseDatagram classifies and parses one raw UDP-style datagram. RTP
// uses an even version-2 payload-type; RTCP uses an odd one (200 = SR).
func ParseDatagram(raw []byte) (Datagram, error) {
	if len(raw) < 2 {
		return Datagram{}, model.Newf(model.KindPacket, "datagram",
			"truncated datagram: %d bytes < 2", len(raw))
	}
	version := raw[0] >> 6
	if version != 2 {
		return Datagram{}, model.Newf(model.KindPacket, "datagram", "unsupported RTP version %d", version)
	}
	payloadType := raw[1]

	if payloadType == 200 { // RTCP Sender Report
		sr, err := parseSenderReport(raw)
		if err != nil {
			return Datagram{}, err
		}
		return Datagram{Kind: KindRTCP, Report: sr}, nil
	}

	var p rtp.Packet
	if err := p.Unmarshal(raw); err != nil {
		return Datagram{}, model.New(model.KindPacket, "rtp", "malformed RTP packet", err)
	}
	return Datagram{
		Kind: KindRTP,
		Packet: model.Packet{
			SSRC:        p.SSRC,
			Sequence:    p.Header.SequenceNumber,
			Timestamp:   p.Header.Timestamp,
			PayloadType: p.Header.PayloadType,
			Marker:      p.Header.Marker,
			Payload:     append([]byte(nil), p.Payload...),
		},
	}, nil
}

// parseSenderReport decodes an RTCP SR (PT 200). A report whose length or
// NTP field is implausible is returned with Valid=false rather than dropped,
// so the pipeline can account for corrupted control traffic.
func parseSenderReport(raw []byte) (model.SenderReport, error) {
	if len(raw) < srHeaderLen {
		return model.SenderReport{}, model.Newf(model.KindPacket, "rtcp",
			"truncated RTCP SR: %d bytes < %d", len(raw), srHeaderLen)
	}
	rc := raw[0] & 0x1f
	minLen := srHeaderLen + int(rc)*24
	if len(raw) < minLen {
		return model.SenderReport{}, model.Newf(model.KindPacket, "rtcp",
			"truncated RTCP SR reception blocks: %d < %d", len(raw), minLen)
	}
	sr := model.SenderReport{
		SSRC:        u32be(raw[4:8]),
		NTPTime:     u64be(raw[8:16]),
		RTPTime:     u32be(raw[16:20]),
		PacketCount: u32be(raw[20:24]),
		OctetCount:  u32be(raw[24:28]),
		Valid:       true,
	}
	// NTP seconds before year 2000 are treated as corruption for this
	// archive (all fixtures are contemporary).
	if sr.NTPTime>>32 < 3155673600 {
		sr.Valid = false
	}
	if sr.NTPTime == 0 && sr.RTPTime == 0 {
		sr.Valid = false
	}
	return sr, nil
}

// IsTruncated reports whether an error is a packet/RTCP parse failure that
// the fixture generator deliberately injects.
func IsTruncated(err error) bool {
	var me *model.Error
	if errors.As(err, &me) {
		return me.Kind == model.KindPacket
	}
	return false
}

func u32be(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func u64be(b []byte) uint64 {
	return uint64(u32be(b[0:4]))<<32 | uint64(u32be(b[4:8]))
}
