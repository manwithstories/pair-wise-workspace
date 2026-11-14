package tsslice

import "io"

const (
	PacketSize = 188
	SyncByte   = 0x47

	PIDPAT   = 0x0000
	PIDPMT   = 0x0100
	PIDVideo = 0x0101
	PIDAudio = 0x0102
)

// Muxer writes a minimal MPEG-TS stream (H.264 + AAC, PAT/PMT, PES with PTS/PCR).
type Muxer struct {
	w  io.Writer
	cc map[uint16]uint8
}

func NewMuxer(w io.Writer) *Muxer { return &Muxer{w: w, cc: map[uint16]uint8{}} }

// writePacket emits one 188-byte TS packet. adaptation holds the bytes
// following the adaptation_field_length byte (flags + data + stuffing).
func (m *Muxer) writePacket(pid uint16, pusi bool, adaptation, payload []byte) error {
	pkt := make([]byte, PacketSize)
	pkt[0] = SyncByte
	if pusi {
		pkt[1] = 0x40
	}
	pkt[1] |= byte(pid >> 8)
	pkt[2] = byte(pid)
	pkt[3] = 0x30 | (m.cc[pid] & 0x0F) // adaptation + payload
	m.cc[pid]++
	pkt[4] = byte(len(adaptation))
	copy(pkt[5:], adaptation)
	copy(pkt[5+len(adaptation):], payload)
	_, err := m.w.Write(pkt)
	return err
}

// WritePATPMT emits the program association and program map tables.
func (m *Muxer) WritePATPMT() error {
	pat := []byte{0x00, 0xB0, 0x0D, 0x00, 0x01, 0xC1, 0x00, 0x00,
		0x00, 0x01, 0xE1, 0x00, 0, 0, 0, 0}
	pmt := []byte{0x02, 0xB0, 0x17, 0x00, 0x01, 0xC1, 0x00, 0x00,
		0xE1, 0x01, 0xF0, 0x00,
		0x1B, 0xE1, 0x01, 0xF0, 0x00, // H.264 video
		0x0F, 0xE1, 0x02, 0xF0, 0x00, // AAC audio
		0, 0, 0, 0}
	if err := m.writeSection(PIDPAT, pat); err != nil {
		return err
	}
	return m.writeSection(PIDPMT, pmt)
}

func (m *Muxer) writeSection(pid uint16, section []byte) error {
	payload := append([]byte{0x00}, section...) // pointer_field
	content := []byte{0x00}
	capacity := 183 - len(content)
	chunk := min(len(payload), capacity)
	adaptation := append(content, stuffing(capacity-chunk)...)
	return m.writePacket(pid, true, adaptation, payload[:chunk])
}

// WritePES packetizes one PES (PTS always present). Keyframe PES carry the
// random-access indicator and a PCR in the adaptation field.
func (m *Muxer) WritePES(pid uint16, streamID byte, pts int64, keyframe bool, payload []byte) error {
	pes := make([]byte, 0, 14+len(payload))
	pes = append(pes, 0x00, 0x00, 0x01, streamID)
	length := 8 + len(payload)
	if length > 0xFFFF {
		length = 0
	}
	pes = append(pes, byte(length>>8), byte(length))
	pes = append(pes, 0x80, 0x80, 0x05) // '10', PTS flag, header length 5
	pes = append(pes, encodePTS(pts)...)
	pes = append(pes, payload...)

	first := true
	for len(pes) > 0 {
		var content []byte
		if first && keyframe {
			content = append([]byte{0x50}, encodePCR(pts)...) // RAI + PCR
		} else {
			content = []byte{0x00}
		}
		capacity := 183 - len(content)
		chunk := min(len(pes), capacity)
		adaptation := append(content, stuffing(capacity-chunk)...)
		if err := m.writePacket(pid, first, adaptation, pes[:chunk]); err != nil {
			return err
		}
		pes = pes[chunk:]
		first = false
	}
	return nil
}

func stuffing(n int) []byte {
	if n <= 0 {
		return nil
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = 0xFF
	}
	return b
}

func encodePTS(pts int64) []byte {
	return []byte{
		0x20 | byte((pts>>29)&0x0E) | 0x01,
		byte(pts >> 22),
		byte((pts>>14)&0xFE) | 0x01,
		byte(pts >> 7),
		byte((pts<<1)&0xFE) | 0x01,
	}
}

func encodePCR(base int64) []byte {
	return []byte{
		byte(base >> 25), byte(base >> 17), byte(base >> 9), byte(base >> 1),
		byte(base<<7) | 0x7E, 0x00,
	}
}
