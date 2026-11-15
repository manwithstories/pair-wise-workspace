package fixture

import (
	"bufio"
)

type packetWriter struct {
	w     *bufio.Writer
	cc    map[uint16]uint8
	index uint64
}

func newPacketWriter(w *bufio.Writer) *packetWriter {
	return &packetWriter{w: w, cc: make(map[uint16]uint8)}
}

func (p *packetWriter) write(pid uint16, pusi bool, payload []byte, drop bool) bool {
	if len(payload) > 184 {
		panic("fixture: TS payload exceeds 184 bytes")
	}
	cc := p.cc[pid]
	p.cc[pid] = (cc + 1) & 0x0f
	if drop {
		return false
	}
	var pkt [188]byte
	pkt[0] = 0x47
	if pusi {
		pkt[1] |= 0x40
	}
	pkt[1] |= byte(pid>>8) & 0x1f
	pkt[2] = byte(pid)
	pkt[3] = 0x10 | cc
	o := 4
	copy(pkt[o:], payload)
	for i := o + len(payload); i < len(pkt); i++ {
		pkt[i] = 0xff
	}
	_, _ = p.w.Write(pkt[:])
	p.index++
	return true
}

func (p *packetWriter) writeSection(pid uint16, section []byte) error {
	first := true
	for len(section) > 0 {
		payload := make([]byte, 0, 184)
		pusi := first
		if first {
			payload = append(payload, 0)
			first = false
		}
		n := 184 - len(payload)
		if n > len(section) {
			n = len(section)
		}
		payload = append(payload, section[:n]...)
		section = section[n:]
		p.write(pid, pusi, payload, false)
	}
	return nil
}

func writePES(streamID uint8, pts int64, es []byte) []byte {
	var b []byte
	b = append(b, 0, 0, 1, streamID, 0, 0, 0x80, 0x80, 0)
	b = append(b, 0x21|byte((pts>>29)&0x0e), byte(pts>>22), byte((pts>>14)&0xfe)|1, byte(pts>>7), byte((pts<<1)&0xfe)|1)
	b = append(b, es...)
	return b
}

func videoES(key bool, marker byte) []byte {
	if key {
		return []byte{0, 0, 0, 1, 0x09, 0xf0, 0, 0, 0, 1, 0x67, 0x42, 0x00, 0x1e, 0, 0, 0, 1, 0x68, 0xce, 0x3c, 0x80, 0, 0, 0, 1, 0x65, marker}
	}
	return []byte{0, 0, 0, 1, 0x41, marker, 0xaa, 0xbb}
}

func audioES(marker byte) []byte {
	return []byte{0xff, 0xf1, marker, 0x80, 0x01, 0xaa, 0xbb, 0xcc}
}
