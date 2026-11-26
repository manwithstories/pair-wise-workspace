package sample

import (
	"encoding/binary"
)

// Frame is one coded video frame and the TS packets that carry it.
type Frame struct {
	Index     int // frame index from program start
	RTPTime   uint32
	IsIDR     bool
	NALType   byte
	TSPackets [][]byte // each exactly TSPacketSize bytes, video PID
}

// continuity counters kept per PID while one rendition's stream is built.
type ccState struct {
	pat   uint8
	pmt   uint8
	video uint8
}

// buildFrame emits the TSPerFrame video TS packets for frame index fi. The
// first packet carries payload_unit_start_indicator and a minimal PES whose
// payload begins with an Annex-B start code and an auditable NAL header whose
// type distinguishes IDR from non-IDR frames.
func buildFrame(fi int, cc *ccState, tspf int) Frame {
	isIDR := fi%IDRPeriodFrame == 0
	nalType := byte(NALNonIDR)
	if isIDR {
		nalType = NALIDR
	}
	rtpTime := uint32(uint64(fi) * TicksPerFrame)

	pkts := make([][]byte, 0, tspf)
	for j := 0; j < tspf; j++ {
		b := make([]byte, TSPacketSize)
		pusi := j == 0
		writeHeader(b, PIDVideo, pusi, true, cc.video)
		if pusi {
			// Adaptation field carrying a PCR, then a minimal PES packet.
			b[3] = 0x30 | (cc.video & 0x0F) // adaptation + payload
			b[4] = 7                         // adaptation field length
			b[5] = 0x10                      // PCR flag
			putPCR(b[6:12], pcrBase(fi))
			putPES(b[12:], fi, nalType)
		} else {
			// Continuation packets carry a deterministic, seed-derived payload
			// that still parses as ordinary TS (no further framing needed).
			fillVideoPayload(b[4:], fi, j, nalType)
		}
		pkts = append(pkts, b)
		cc.video = (cc.video + 1) & 0x0F
	}
	return Frame{Index: fi, RTPTime: rtpTime, IsIDR: isIDR, NALType: nalType, TSPackets: pkts}
}

// pcrBase derives a 27 MHz-compatible PCR base from the frame index.
func pcrBase(fi int) uint64 { return uint64(fi) * TicksPerFrame * 300 }

func putPCR(b []byte, base uint64) {
	// 33-bit PCR base (90 kHz units) packed into 6 bytes per ISO 13818-1,
	// reserved bits set and a zero 9-bit extension.
	b[0] = byte(base >> 25)
	b[1] = byte(base >> 17)
	b[2] = byte(base >> 9)
	b[3] = byte(base >> 1)
	b[4] = byte(base<<7) | 0x7E
	b[5] = 0x00
}

// putPES writes a minimal video PES packet followed by an Annex-B NAL unit so
// the segmenter can detect frame boundaries (PUSI) and keyframes (NAL type).
// b is the TS payload region beginning immediately after the adaptation field.
func putPES(b []byte, fi int, nalType byte) {
	end := len(b)
	// PES start code 0x000001E0, streaming-id 0xE0.
	b[0], b[1], b[2], b[3] = 0x00, 0x00, 0x01, 0xE0
	binary.BigEndian.PutUint16(b[4:6], 0x0010) // PES packet length (placeholder)
	b[6] = 0x80                                 // marker bits
	b[7] = 0x00                                 // no PES header flags beyond PTS
	b[8] = 0x05                                 // PES header data length
	putPTS(b[9:15], uint64(fi)*TicksPerFrame*300)
	// Annex-B start code then a one-byte NAL header (forbidden=0, nal_ref_idc,
	// type). nal_ref_idc is set high for IDR as real encoders do.
	b[14], b[15], b[16] = 0x00, 0x00, 0x01
	refIDC := byte(0x20)
	if nalType == NALIDR {
		refIDC = 0x60
	}
	b[17] = refIDC | (nalType & 0x1F)
	// Remaining bytes: deterministic slice content for hashing.
	for i := 18; i < end; i++ {
		b[i] = byte((fi*31 + i*7) & 0xFF)
	}
}

// putPTS writes a 33-bit PTS in the 5-byte MPEG-TS PES encoding.
func putPTS(b []byte, pts uint64) {
	b[0] = 0x21
	b[1] = byte(pts>>29) & 0x0E
	b[2] = byte(pts >> 22)
	b[3] = byte(pts>>14) | 0x01
	b[4] = byte(pts >> 7)
	b[5] = byte(pts<<1) | 0x01
}

// fillVideoPayload writes deterministic continuation bytes.
func fillVideoPayload(b []byte, fi, j int, nalType byte) {
	for i := range b {
		b[i] = byte((fi*31 + j*17 + i*7 + int(nalType)) & 0xFF)
	}
}

// psPacket returns the periodic PAT/PMT pair with incrementing counters.
func psPacket(cc *ccState) [][]byte {
	out := [][]byte{patPacket(cc.pat), pmtPacket(cc.pmt)}
	cc.pat = (cc.pat + 1) & 0x0F
	cc.pmt = (cc.pmt + 1) & 0x0F
	return out
}
