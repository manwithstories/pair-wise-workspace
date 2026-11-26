// Package fixture generates deterministic in-process sample media: HLS/DASH
// manifests, MPEG-TS and CMAF segments, plus deliberately corrupted variants
// (truncation, bad box length, missing init, wrong duration) used by the tests
// and the demo suite.
package fixture

import "fmt"

// The generator writes small but structurally valid containers: just enough of
// PAT/PMT/PES and ISO-BMFF to exercise every demuxer branch, with deterministic
// payload bytes so payload hashes are stable across runs.

const (
	// TSPacketSize is the MPEG-TS transport packet length.
	TSPacketSize = 188
	// VideoTimescale and AudioTimescale mirror the timescales real delivery
	// packages use, so the rational clock stress is realistic.
	VideoTimescale = 90000
	AudioTimescale = 48000
	// AudioSamplesPerFrame is one AAC-LC frame.
	AudioSamplesPerFrame = 1024
)

// PIDs used by the generated transport stream.
const (
	PIDVideo = 0x0100
	PIDAudio = 0x0101
	PIDPMT   = 0x1000
)

// TSWriter accumulates 188-byte packets, tracking continuity counters per PID.
type TSWriter struct {
	buf   []byte
	cc    map[uint16]uint8
	force map[uint16]uint8
}

// NewTSWriter returns an empty transport-stream writer.
func NewTSWriter() *TSWriter {
	return &TSWriter{cc: map[uint16]uint8{}, force: map[uint16]uint8{}}
}

// ForceCC sets the continuity counter for the next packet of pid, used by the
// corrupt-fixture builder to inject a break without special-casing the writer.
func (w *TSWriter) ForceCC(pid uint16, cc uint8) { w.force[pid] = cc }

// Bytes returns the accumulated transport stream.
func (w *TSWriter) Bytes() []byte { return w.buf }

// WritePAT emits the program association table naming PIDPMT.
func (w *TSWriter) WritePAT() {
	w.writeSection(0x0000, patSection())
}

// WritePMT emits the program map table for one AVC and one AAC stream.
func (w *TSWriter) WritePMT() {
	w.writeSection(PIDPMT, pmtSection())
}

// writeSection wraps a PSI section in a pointer field and splits it across
// packets, starting a new packet whenever the section is present.
func (w *TSWriter) writeSection(pid uint16, section []byte) {
	w.writeUnits(pid, append([]byte{0x00}, section...))
}

// WritePES emits one PES packet carrying PTS (and DTS when withDTS is set).
func (w *TSWriter) WritePES(pid uint16, pts, dts int64, payload []byte, withDTS bool) {
	w.writeUnits(pid, append(pesHeader(pts, dts, withDTS, len(payload)), payload...))
}

// writeUnits splits payload across transport packets, first one marked
// payload_unit_start. Continuity counters advance per packet.
func (w *TSWriter) writeUnits(pid uint16, payload []byte) {
	if v, ok := w.force[pid]; ok {
		w.cc[pid] = v
		delete(w.force, pid)
	}
	first := true
	for len(payload) > 0 || first {
		cc := w.cc[pid]
		w.cc[pid] = (cc + 1) & 0x0F
		n := len(payload)
		if n > TSPacketSize-4 {
			n = TSPacketSize - 4
		}
		af := (TSPacketSize - 4) - n
		pkt := make([]byte, TSPacketSize)
		pkt[0] = 0x47
		b := byte(pid>>8) & 0x1F
		if first {
			b |= 0x40
		}
		pkt[1] = b
		pkt[2] = byte(pid)
		pkt[3] = 1<<4 | cc
		if af > 0 {
			pkt[3] = 3<<4 | cc
			pkt[4] = byte(af - 1)
			if af > 1 {
				pkt[5] = 0x00
				for i := 6; i < 4+af; i++ {
					pkt[i] = 0xFF
				}
			}
		}
		copy(pkt[4+af:], payload[:n])
		w.buf = append(w.buf, pkt...)
		payload = payload[n:]
		first = false
	}
}

// patSection builds the PAT body followed by its CRC.
func patSection() []byte {
	body := []byte{
		0x00,       // table_id
		0x00, 0x00, // section length placeholder
		0x00, 0x01, // transport_stream_id
		0xC1,       // version 0, current_next_indicator 1
		0x00,       // section_number
		0x00,       // last_section_number
		0x00, 0x01, // program_number 1
		0xE0 | byte(PIDPMT>>8), byte(PIDPMT & 0xFF),
	}
	return withSectionLength(body)
}

// pmtSection builds the PMT body followed by its CRC.
func pmtSection() []byte {
	body := []byte{
		0x02,       // table_id
		0x00, 0x00, // section length placeholder
		0x00, 0x01, // program_number
		0xC1,       // version, current_next_indicator
		0x00, 0x00, // section_number, last_section_number
		0xE0 | byte(PIDVideo>>8), byte(PIDVideo & 0xFF), // PCR PID
		0xF0, 0x00, // program_info_length
		0x1B, 0xE0 | byte(PIDVideo>>8), byte(PIDVideo & 0xFF), 0xF0, 0x00, // AVC video
		0x0F, 0xE0 | byte(PIDAudio>>8), byte(PIDAudio & 0xFF), 0xF0, 0x00, // AAC audio
	}
	return withSectionLength(body)
}

// withSectionLength fills the 12-bit length field including the trailing CRC.
func withSectionLength(body []byte) []byte {
	slen := len(body) - 3 + 4
	body[1] = 0xF0 | byte(slen>>8)&0x0F
	body[2] = byte(slen & 0xFF)
	return append(body, crc32mpeg(body)...)
}

// crc32mpeg computes the MPEG-2 CRC-32 (poly 0x04C11DB7, no reflection).
func crc32mpeg(b []byte) []byte {
	var crc uint32 = 0xFFFFFFFF
	for _, v := range b {
		crc ^= uint32(v) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return []byte{byte(crc >> 24), byte(crc >> 16), byte(crc >> 8), byte(crc)}
}

// pesHeader builds a PES header with PTS, and DTS when requested.
func pesHeader(pts, dts int64, withDTS bool, payloadLen int) []byte {
	flags := byte(0x80) // PTS present
	hlen := 5
	if withDTS {
		flags |= 0x40
		hlen = 10
	}
	h := make([]byte, 9+hlen)
	h[0], h[1], h[2] = 0x00, 0x00, 0x01
	h[3] = 0xBD // carried stream type comes from the PMT PID mapping
	h[4], h[5] = byte((payloadLen+3+hlen)>>8), byte((payloadLen+3+hlen)&0xFF)
	h[6] = 0x84
	h[7] = flags
	h[8] = byte(hlen)
	putTimestamp(h[9:14], 0x2, pts)
	if withDTS {
		putTimestamp(h[14:19], 0x1, dts)
	}
	return h
}

// putTimestamp encodes a 33-bit PTS/DTS field with its 4-bit prefix
// ("0010" PTS only, "0011" PTS with DTS following, "0001" DTS).
func putTimestamp(b []byte, prefix byte, v int64) {
	b[0] = prefix<<4 | byte((v>>30)&0x07)<<1 | 0x01
	b[1] = byte(v >> 22)
	b[2] = byte((v>>15)&0x7F)<<1 | 0x01
	b[3] = byte(v >> 7)
	b[4] = byte(v&0x7F)<<1 | 0x01
}

// CheckTSFraming verifies 188-byte alignment for callers that want an early
// error before handing bytes to the demuxer.
func CheckTSFraming(data []byte) error {
	if len(data)%TSPacketSize != 0 {
		return fmt.Errorf("segment length %d is not a multiple of %d", len(data), TSPacketSize)
	}
	return nil
}
