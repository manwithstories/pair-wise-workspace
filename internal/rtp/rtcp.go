package rtp

import (
	"encoding/binary"

	"medpipe/internal/clock"
	"medpipe/internal/fault"
)

// RTCP packet types we care about.
const (
	rtcpPTSR = 200 // sender report
)

// SenderReport is the parsed content of an RTCP sender report. It
// anchors a track's RTP timestamp domain to the shared (NTP) timeline
// and carries the sender's cumulative packet count, which the pipeline
// uses to detect tail loss.
type SenderReport struct {
	SSRC         uint32
	NTPSec       uint32
	NTPFrac      uint32
	RTPTimestamp uint32
	PacketCount  uint32
	OctetCount   uint32
}

// NTPTime returns the report's NTP timestamp as exact seconds on the
// shared timeline (fraction denominator 2^32).
func (sr *SenderReport) NTPTime() clock.Rat {
	sec := clock.New(int64(sr.NTPSec), 1)
	frac := clock.New(int64(sr.NTPFrac), 1<<32)
	return sec.Add(frac)
}

// ParseSenderReport parses one RTCP sender-report packet. Compound
// packets and other RTCP types are rejected with a located error; the
// generator only ever emits plain SR packets.
func ParseSenderReport(raw []byte, track uint32, off int64) (*SenderReport, error) {
	if len(raw) < 28 {
		return nil, fault.New(fault.KindCorrupt, int64(track), fault.Unknown, off, true,
			"rtcp sender report too short")
	}
	version := raw[0] >> 6
	if version != 2 {
		return nil, fault.New(fault.KindCorrupt, int64(track), fault.Unknown, off, true,
			"rtcp bad version")
	}
	pt := raw[1]
	if pt != rtcpPTSR {
		return nil, fault.New(fault.KindCorrupt, int64(track), fault.Unknown, off, true,
			"rtcp not a sender report")
	}
	length := int(binary.BigEndian.Uint16(raw[2:4])+1) * 4
	if length > len(raw) {
		return nil, fault.New(fault.KindCorrupt, int64(track), fault.Unknown, off, true,
			"rtcp length exceeds record")
	}
	return &SenderReport{
		SSRC:         binary.BigEndian.Uint32(raw[4:8]),
		NTPSec:       binary.BigEndian.Uint32(raw[8:12]),
		NTPFrac:      binary.BigEndian.Uint32(raw[12:16]),
		RTPTimestamp: binary.BigEndian.Uint32(raw[16:20]),
		PacketCount:  binary.BigEndian.Uint32(raw[20:24]),
		OctetCount:   binary.BigEndian.Uint32(raw[24:28]),
	}, nil
}

// MarshalSenderReport encodes a sender report. Used by the sample
// generator; it is the exact inverse of ParseSenderReport.
func MarshalSenderReport(sr *SenderReport) []byte {
	raw := make([]byte, 28)
	raw[0] = 2 << 6 // version 2, no padding, rc 0
	raw[1] = rtcpPTSR
	binary.BigEndian.PutUint16(raw[2:4], 28/4-1)
	binary.BigEndian.PutUint32(raw[4:8], sr.SSRC)
	binary.BigEndian.PutUint32(raw[8:12], sr.NTPSec)
	binary.BigEndian.PutUint32(raw[12:16], sr.NTPFrac)
	binary.BigEndian.PutUint32(raw[16:20], sr.RTPTimestamp)
	binary.BigEndian.PutUint32(raw[20:24], sr.PacketCount)
	binary.BigEndian.PutUint32(raw[24:28], sr.OctetCount)
	return raw
}
