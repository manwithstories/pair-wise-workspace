package capture

import (
	"encoding/binary"

	"mediarig/internal/media"
)

const rtcpSenderReportType byte = 200

// SenderReport is the subset of an RTCP SR required for clock alignment.
type SenderReport struct {
	SSRC         uint32
	NTPMSW       uint32
	NTPLSW       uint32
	RTPTimestamp uint32
	PacketCount  uint32
	OctetCount   uint32
	NTPSeconds   media.Rational
	RecordIndex  int
}

// NTPFraction converts the 64-bit NTP timestamp to exact seconds. The NTP
// epoch offset is irrelevant to relative stream alignment.
func ntpToRational(msw, lsw uint32) media.Rational {
	whole := int64(msw)
	frac := media.MustRat(int64(lsw), 1<<32)
	return media.MustRat(whole, 1).Add(frac)
}

// ParseSenderReports parses every RTCP packet in one compound record and
// returns SR blocks. Other RTCP packet types are accepted and ignored.
func ParseSenderReports(rec Record) ([]SenderReport, error) {
	if rec.Type != RecordRTCP {
		return nil, media.Corrupt(media.StageRTCP, "record type %d is not RTCP", rec.Type)
	}
	b := rec.Payload
	var reports []SenderReport
	for off := 0; off+4 <= len(b); {
		first := b[off]
		version := first >> 6
		if version != 2 {
			return nil, media.WithRecordIndex(
				media.Corrupt(media.StageRTCP, "unsupported RTCP version %d", version), rec.Index)
		}
		pktType := b[off+1]
		wordLen := int(binary.BigEndian.Uint16(b[off+2 : off+4]))
		pktLen := (wordLen + 1) * 4
		if wordLen == 0 || off+pktLen > len(b) {
			return nil, media.WithRecordIndex(
				media.Corrupt(media.StageRTCP, "illegal RTCP length field %d at offset %d", wordLen, off),
				rec.Index)
		}
		if pktType == rtcpSenderReportType {
			if pktLen < 28 {
				return nil, media.WithRecordIndex(
					media.Corrupt(media.StageRTCP, "sender report too short: %d bytes", pktLen), rec.Index)
			}
			p := b[off:]
			sr := SenderReport{
				SSRC:         binary.BigEndian.Uint32(p[4:8]),
				NTPMSW:       binary.BigEndian.Uint32(p[8:12]),
				NTPLSW:       binary.BigEndian.Uint32(p[12:16]),
				RTPTimestamp: binary.BigEndian.Uint32(p[16:20]),
				PacketCount:  binary.BigEndian.Uint32(p[20:24]),
				OctetCount:   binary.BigEndian.Uint32(p[24:28]),
				RecordIndex:  rec.Index,
			}
			sr.NTPSeconds = ntpToRational(sr.NTPMSW, sr.NTPLSW)
			reports = append(reports, sr)
		}
		off += pktLen
	}
	return reports, nil
}
