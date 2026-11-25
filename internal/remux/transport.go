// Package remux produces lossless TS and CMAF/fMP4 outputs.
package remux

import (
	"mediqc/internal/media"
	"mediqc/internal/ts"
)

const (
	TSPMTPID    uint16 = 0x1000
	tsPacketLen        = 188
	tsPayload          = 184
	ptsMod             = 1 << 33
)

// TSOptions controls deterministic TS output.
type TSOptions struct {
	PMTPID     uint16
	PMTVersion byte
}

// BuildTS muxes PSI and PES packets without transcoding sample payloads.
func BuildTS(tracks []media.Track, samples []media.Sample, opts TSOptions) ([]byte, *media.Error) {
	if opts.PMTPID == 0 {
		opts.PMTPID = TSPMTPID
	}
	pmt := buildPMTSection(opts.PMTPID, opts.PMTVersion, tracks)
	pat := buildPATSection(opts.PMTPID, opts.PMTVersion)
	w := &tsWriter{cc: map[uint16]byte{}}
	w.writePSIPacket(ts.PATPID, pat, 0)
	w.writePSIPacket(opts.PMTPID, pmt, 0)
	for _, s := range samples {
		track := findTrack(tracks, s.TrackID, s.PID)
		streamID := byte(0xE0)
		if track != nil && track.Kind == media.TrackAudio {
			streamID = 0xC0
		}
		pes := buildPES(streamID, uint64(s.PTS%ptsMod), uint64(s.DTS%ptsMod), s.Data)
		w.writePES(s.PID, pes)
	}
	return w.buf, nil
}

type tsWriter struct {
	buf []byte
	cc  map[uint16]byte
}

func (w *tsWriter) nextCC(pid uint16) byte {
	v := w.cc[pid]
	w.cc[pid] = (v + 1) & 0x0f
	return v
}

func (w *tsWriter) writePSIPacket(pid uint16, section []byte, cc byte) {
	pkt := make([]byte, tsPacketLen)
	pkt[0] = ts.SyncByte
	pkt[1] = 0x40 | byte((pid>>8)&0x1f) // PUSI
	pkt[2] = byte(pid)
	pkt[3] = 0x10 | (cc & 0x0f)
	pkt[4] = 0x00 // pointer field
	n := copy(pkt[5:], section)
	for i := 5 + n; i < tsPacketLen; i++ {
		pkt[i] = 0xff
	}
	w.buf = append(w.buf, pkt...)
	w.cc[pid] = (cc + 1) & 0x0f
}

func (w *tsWriter) writePES(pid uint16, pes []byte) {
	first := true
	for len(pes) > 0 {
		pkt := make([]byte, tsPacketLen)
		pkt[0] = ts.SyncByte
		pkt[1] = byte((pid >> 8) & 0x1f)
		pkt[2] = byte(pid)
		cc := w.nextCC(pid)
		if first {
			pkt[1] |= 0x40
		}
		n := len(pes)
		if n > tsPayload {
			n = tsPayload
		}
		if n == tsPayload {
			pkt[3] = 0x10 | cc
			copy(pkt[4:], pes[:n])
		} else {
			alen := 183 - n
			pkt[3] = 0x30 | cc
			pkt[4] = byte(alen)
			if alen > 0 {
				pkt[5] = 0x00 // adaptation flags: no PCR
				for i := 6; i < 5+int(alen); i++ {
					pkt[i] = 0xff
				}
			}
			copy(pkt[5+int(alen):], pes[:n])
		}
		w.buf = append(w.buf, pkt...)
		pes = pes[n:]
		first = false
	}
}

func buildPATSection(pmtPID uint16, version byte) []byte {
	body := []byte{
		0x00,       // table_id PAT
		0xb0, 0x0d, // syntax indicator + section_length=13
		0x00, 0x01, // transport_stream_id
		0xc1 | ((version & 0x1f) << 1), // reserved + version + current_next
		0x00, 0x00,                     // section/last number
		0x00, 0x01, // program_number=1
		0xe0 | byte((pmtPID>>8)&0x1f), byte(pmtPID),
	}
	crc := ts.CRC32MPEG(body)
	out := append(body, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
	return out
}

func buildPMTSection(pmtPID uint16, version byte, tracks []media.Track) []byte {
	// section_length is updated after stream entries are assembled.
	body := []byte{
		0x02,
		0xb0, 0x00,
		0x00, 0x01, // program number
		0xc1 | ((version & 0x1f) << 1),
		0x00, 0x00,
		0xe0 | byte((pmtPID>>8)&0x1f), byte(pmtPID), // PCR PID uses PMT PID? replace below
		0xf0, 0x00, // program descriptors length 0
	}
	// Use first video PID as PCR PID when available.
	pcrPID := pmtPID
	for _, tr := range tracks {
		if tr.Kind == media.TrackVideo {
			pcrPID = tr.PID
		}
	}
	body[8] = 0xe0 | byte((pcrPID>>8)&0x1f)
	body[9] = byte(pcrPID)
	for _, tr := range tracks {
		streamType := byte(0x0f)
		if tr.Codec == media.CodecH264 {
			streamType = 0x1b
		}
		body = append(body, streamType, 0xe0|byte((tr.PID>>8)&0x1f), byte(tr.PID), 0xf0, 0x00)
	}
	sectionLength := len(body) + 4 - 3 // includes CRC but excludes first 3 bytes
	body[1] = 0xb0 | byte((sectionLength>>8)&0x0f)
	body[2] = byte(sectionLength)
	crc := ts.CRC32MPEG(body)
	return append(body, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
}

func buildPES(streamID byte, pts, dts uint64, data []byte) []byte {
	headerDataLen := byte(10)
	packetLength := 3 + int(headerDataLen) + len(data)
	out := []byte{0x00, 0x00, 0x01, streamID, byte(packetLength >> 8), byte(packetLength), 0x80, 0xc0, headerDataLen}
	out = append(out, encodeTimestamp(pts, 0x02)...)
	out = append(out, encodeTimestamp(dts, 0x03)...)
	out = append(out, data...)
	return out
}

func encodeTimestamp(v uint64, prefix byte) []byte {
	return []byte{
		(prefix << 4) | byte(((v>>30)&0x07)<<1) | 0x01,
		byte(v >> 22),
		byte((((v >> 15) & 0x7f) << 1) | 0x01),
		byte(v >> 7),
		byte(((v & 0x7f) << 1) | 0x01),
	}
}

func findTrack(tracks []media.Track, id int, pid uint16) *media.Track {
	for i := range tracks {
		if tracks[i].ID == id || tracks[i].PID == pid {
			return &tracks[i]
		}
	}
	return nil
}

// FormatBitratePPM returns integer parts-per-million deviation.
func FormatBitratePPM(target, actual int64) int64 {
	if target == 0 {
		return 0
	}
	return int64(float64(actual-target) / float64(target) * 1_000_000)
}
