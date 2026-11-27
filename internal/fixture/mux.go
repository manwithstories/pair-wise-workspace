package fixture

import (
	"encoding/binary"

	"tsqc/internal/ts"
)

func (b *builder) writePSI(pid uint16, sec []byte) {
	payload := append([]byte{0x00}, sec...)
	for len(payload) > 0 {
		n := len(payload)
		if n > 184 {
			n = 184
		}
		b.packet(pid, len(payload) == n, payload[:n], -1)
		payload = payload[n:]
	}
}

func makeSection(tableID uint8, ext uint16, version uint8, body []byte) []byte {
	length := len(body) + 5 + 4
	sec := make([]byte, 3+length)
	sec[0] = tableID
	sec[1] = 0xB0 | byte(length>>8&0x0f)
	sec[2] = byte(length)
	binary.BigEndian.PutUint16(sec[3:], ext)
	sec[5] = 0xC1 | version<<1&0x1e
	copy(sec[8:], body)
	binary.BigEndian.PutUint32(sec[len(sec)-4:], ts.CRC32(sec[:len(sec)-4]))
	return sec
}

func (b *builder) writePAT(version uint8) {
	pmt := uint16(PMTPID)
	body := []byte{0x00, 0x01, byte(pmt>>8) | 0xE0, byte(pmt)}
	b.writePSI(ts.PIDPAT, makeSection(0x00, 1, version, body))
}

func (b *builder) writePMT(version uint8) {
	vpid, apid := uint16(VideoPID), uint16(AudioPID)
	body := []byte{byte(vpid>>8) | 0xE0, byte(vpid), 0xF0, 0x00}
	body = append(body, ts.StreamH264, byte(vpid>>8)|0xE0, byte(vpid), 0xF0, 0x00)
	body = append(body, ts.StreamAAC, byte(apid>>8)|0xE0, byte(apid), 0xF0, 0x00)
	b.writePSI(PMTPID, makeSection(0x02, 1, version, body))
}

func (b *builder) writeVideo(pts, pcr int64, key bool, payload []byte, ccJump bool) {
	// Video carries a DTS equal to its PTS, matching common encoders.
	b.curKey = key
	b.writePES(VideoPID, 0xE0, pts, pts, true, payload, pcr, ccJump)
	b.curKey = false
}

func (b *builder) writeAudio(pts int64) {
	payload := make([]byte, 7+64)
	payload[0], payload[1] = 0xFF, 0xF1
	payload[2], payload[3] = 0x50, 0x80
	payload[4], payload[5], payload[6] = 0x00, 0x1F, 0xFC
	for i := 7; i < len(payload); i++ {
		payload[i] = byte(b.rng.Intn(251))
	}
	b.writePES(AudioPID, 0xC0, pts, pts, false, payload, -1, false)
}

func (b *builder) writePES(pid uint16, streamID byte, pts, dts int64, hasDTS bool, data []byte, pcr int64, ccJump bool) {
	hlen := 5
	flags := byte(0x80)
	if hasDTS {
		hlen += 5
		flags |= 0x40
	}
	hdr := make([]byte, 9+hlen)
	hdr[0], hdr[1], hdr[2], hdr[3] = 0, 0, 1, streamID
	total := len(hdr) + len(data)
	binary.BigEndian.PutUint16(hdr[4:], uint16(total-6))
	hdr[6] = 0x80
	hdr[7] = flags
	hdr[8] = byte(hlen)
	putStamp(hdr[9:], pts/300)
	if hasDTS {
		putStamp(hdr[14:], dts/300)
	}
	pes := append(hdr, data...)
	first := true
	for len(pes) > 0 {
		limit := 184
		if first && pcr >= 0 {
			limit = 176
		}
		n := len(pes)
		if n > limit {
			n = limit
		}
		p := pcr
		if !first {
			p = -1
		}
		cc := b.cc[pid]
		if ccJump && first {
			cc = (cc + 3) & 0x0f
		}
		b.packet(pid, first, pes[:n], p)
		if ccJump && first {
			b.cc[pid] = cc
		}
		pes = pes[n:]
		pcr = -1
		first = false
	}
}

func putStamp(b []byte, v int64) {
	u := uint64(v)
	b[0] = 0x21 | byte(u>>29)&0x0e
	b[1] = byte(u >> 22)
	b[2] = 0x01 | byte(u>>14)&0xfe
	b[3] = byte(u >> 7)
	b[4] = 0x01 | byte(u<<1)&0xfe
}

func (b *builder) packet(pid uint16, pusi bool, payload []byte, pcr int64) {
	p := make([]byte, ts.PacketSize)
	p[0] = ts.SyncByte
	p[1] = byte(pid>>8) & 0x1f
	if pusi {
		p[1] |= 0x40
	}
	p[2] = byte(pid)
	cc := b.cc[pid]
	b.cc[pid] = (cc + 1) & 0x0f
	off := 4
	if pcr >= 0 {
		p[3] = 0x30 | cc
		base := uint64(pcr / 300)
		ext := uint16(pcr % 300)
		p[4] = 7
		p[5] = 0x10
		p[6] = byte(base >> 25)
		p[7] = byte(base >> 17)
		p[8] = byte(base >> 9)
		p[9] = byte(base >> 1)
		p[10] = byte(base<<7) | byte(ext>>8)&0x01
		p[11] = byte(ext)
		off = 12
	} else {
		p[3] = 0x10 | cc
	}
	n := copy(p[off:], payload)
	for i := off + n; i < ts.PacketSize; i++ {
		p[i] = 0xFF
	}
	b.out = append(b.out, p...)
	b.pktKey = append(b.pktKey, b.curKey)
}

// injectBadPSI writes a PAT and PMT with broken CRC plus a nonsense version.
func (b *builder) injectBadPSI() {
	pmt := uint16(PMTPID)
	bad := makeSection(0x00, 1, 7, []byte{0x00, 0x01, byte(pmt>>8) | 0xE0, byte(pmt)})
	bad[len(bad)-1] ^= 0xFF
	b.writePSI(ts.PIDPAT, bad)
	vpid := uint16(VideoPID)
	badPMT := makeSection(0x02, 1, 7, []byte{byte(vpid>>8) | 0xE0, byte(vpid), 0xF0, 0x00,
		ts.StreamH264, byte(vpid>>8) | 0xE0, byte(vpid), 0xF0, 0x00})
	badPMT[len(badPMT)-2] ^= 0xFF
	b.writePSI(PMTPID, badPMT)
}
