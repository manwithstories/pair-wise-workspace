// Package fixture generates tiny deterministic local H.264/AAC MPEG-TS assets.
// No external media is used; every byte is derived from the supplied seed.
package fixture

import (
	"encoding/binary"

	"mediad/internal/ingest"
)

const (
	PATPID   = 0x0000
	PMTPID   = 0x1000
	VideoPID = 0x0100
	AudioPID = 0x0101
)

// Writer serializes bounded single-packet PSI/PES units into 188-byte TS packets.
type Writer struct {
	Buf []byte
	cc  map[uint16]byte
}

// NewWriter creates a TS writer with continuity counters starting at zero.
func NewWriter() *Writer {
	return &Writer{cc: map[uint16]byte{}}
}

// JumpCC skips delta continuity counts on the next packet for a PID.
func (w *Writer) JumpCC(pid uint16, delta byte) {
	w.cc[pid] = (w.cc[pid] + delta) & 0x0f
}

// WriteSection packetizes one complete PSI section.
func (w *Writer) WriteSection(pid uint16, section []byte) {
	payload := append([]byte{0x00}, section...)
	if len(payload) < 184 {
		payload = append(payload, make([]byte, 184-len(payload))...)
		// MPEG-TS PSI padding is conventionally 0xff.
		for i := 1 + len(section); i < 184; i++ {
			payload[i] = 0xff
		}
	}
	w.writeRaw(pid, true, 0x10, payload, nil)
}

// WriteNulls appends valid 0x1FFF null stuffing packets. The fixed count is
// used by the demo so packet index 128 is an exact common IDR safe cut.
func (w *Writer) WriteNulls(count int) {
	for i := 0; i < count; i++ {
		pkt := make([]byte, ingest.PacketSize)
		pkt[0] = ingest.SyncByte
		pkt[1] = 0x1f
		pkt[2] = 0xff
		pkt[3] = 0x10
		for j := 4; j < ingest.PacketSize; j++ {
			pkt[j] = 0xff
		}
		w.Buf = append(w.Buf, pkt...)
	}
}

// WritePES writes one PTS-only PES access unit.
func (w *Writer) WritePES(pid uint16, streamID byte, pts uint64, payload []byte, pcr *ingest.PCR) error {
	ptsBytes := encodePTS(pts)
	pes := []byte{0x00, 0x00, 0x01, streamID, 0x00, 0x00, 0x80, 0x80, 0x05}
	pes = append(pes, ptsBytes...)
	pes = append(pes, payload...)
	binary.BigEndian.PutUint16(pes[4:6], uint16(len(pes)-6))
	if len(pes) > 176 {
		return errPESBeyondSinglePacket
	}
	w.writeAligned(pid, pes, pcr)
	return nil
}

func (w *Writer) writeAligned(pid uint16, pes []byte, pcr *ingest.PCR) {
	cc := w.cc[pid]
	w.cc[pid] = (cc + 1) & 0x0f
	hdr := []byte{ingest.SyncByte, byte(pid>>8) | 0x40, byte(pid), 0x30 | cc}
	adaptLen := 183 - len(pes)
	minAdapt := 1
	if pcr != nil {
		minAdapt = 7
	}
	if adaptLen < minAdapt {
		// The generated fixtures stay well below this bound.
		adaptLen = minAdapt
	}
	af := make([]byte, adaptLen)
	for i := range af {
		af[i] = 0xff
	}
	if pcr != nil {
		af[0] = 0x10
		encodePCR(af[1:7], pcr)
	} else {
		af[0] = 0x00
	}
	w.Buf = append(w.Buf, hdr...)
	w.Buf = append(w.Buf, byte(adaptLen))
	w.Buf = append(w.Buf, af...)
	w.Buf = append(w.Buf, pes...)
}

func (w *Writer) writeRaw(pid uint16, pusi bool, adaptationControl byte, payload []byte, pcr []byte) {
	cc := w.cc[pid]
	w.cc[pid] = (cc + 1) & 0x0f
	b1 := byte(pid >> 8)
	if pusi {
		b1 |= 0x40
	}
	hdr := []byte{ingest.SyncByte, b1, byte(pid), adaptationControl | cc}
	w.Buf = append(w.Buf, hdr...)
	if adaptationControl&0x20 != 0 {
		w.Buf = append(w.Buf, byte(len(pcr)))
		w.Buf = append(w.Buf, pcr...)
	}
	w.Buf = append(w.Buf, payload...)
}

func encodePTS(pts uint64) []byte {
	return []byte{
		0x21 | byte(((pts>>30)&0x07)<<1),
		byte((pts >> 22) & 0xff),
		byte(((pts >> 14) & 0xfe) | 0x01),
		byte((pts >> 7) & 0xff),
		byte((pts<<1)&0xfe | 0x01),
	}
}

func encodePCR(b []byte, pcr *ingest.PCR) {
	base := pcr.Base
	ext := pcr.Extension
	b[0] = byte(base >> 25)
	b[1] = byte(base >> 17)
	b[2] = byte(base >> 9)
	b[3] = byte(base >> 1)
	b[4] = byte(base<<7) | 0x7e | byte((ext>>8)&0x01)
	b[5] = byte(ext)
}

// PAT builds a versioned PAT section.
func PAT(version uint8, pmtPID uint16) []byte {
	sec := []byte{0x00, 0xb0, 0x00, 0x00, 0x01, 0xc1 | (version&0x1f)<<1, 0x00, 0x00}
	prog := make([]byte, 4)
	binary.BigEndian.PutUint16(prog[0:2], 1)
	binary.BigEndian.PutUint16(prog[2:4], 0xe000|pmtPID)
	sec = append(sec, prog...)
	return finalizeSection(sec)
}

// PMT builds a versioned PMT section for the fixture H.264/AAC streams.
func PMT(version uint8) []byte {
	sec := []byte{0x02, 0xb0, 0x00, 0x00, 0x01, 0xc1 | (version&0x1f)<<1, 0x00, 0x00}
	pcr := make([]byte, 2)
	binary.BigEndian.PutUint16(pcr, 0xe000|VideoPID)
	sec = append(sec, pcr...)
	sec = append(sec, 0xf0, 0x00) // program info length zero
	for _, s := range []struct {
		streamType byte
		pid        uint16
	}{{0x1b, VideoPID}, {0x0f, AudioPID}} {
		entry := []byte{s.streamType, 0xe0 | byte(s.pid>>8), byte(s.pid), 0xf0, 0x00}
		sec = append(sec, entry...)
	}
	return finalizeSection(sec)
}

func finalizeSection(body []byte) []byte {
	sectionLength := len(body) - 3 + 4 // CRC32 follows the length-scoped body suffix.
	body[1] = 0xb0 | byte(sectionLength>>8)
	body[2] = byte(sectionLength)
	crc := ingest.MPEGCRC32(body)
	crcBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(crcBytes, crc)
	return append(body, crcBytes...)
}

type fixtureError string

func (e fixtureError) Error() string { return string(e) }

const errPESBeyondSinglePacket fixtureError = "fixture PES is too large for one TS packet"
