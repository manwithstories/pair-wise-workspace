package ts

func tsPacket(pid uint16, cc uint8, pusi bool, payload []byte, pcr int64, hasPCR bool) []byte {
	p := make([]byte, PacketSize)
	p[0] = SyncByte
	p[1] = byte(pid >> 8 & 0x1f)
	if pusi {
		p[1] |= 0x40
	}
	p[2] = byte(pid)
	afc := byte(0x10) // payload only
	off := 4
	if hasPCR {
		afc = 0x30
	}
	p[3] = afc | cc&0x0f
	if hasPCR {
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
	}
	n := copy(p[off:], payload)
	for i := off + n; i < PacketSize; i++ {
		p[i] = 0xFF
	}
	return p
}

func pesHeader(streamID uint8, pts, dts int64, hasDTS bool) []byte {
	flags := byte(0x80)
	hlen := 5
	if hasDTS {
		flags |= 0x40
		hlen += 5
	}
	h := []byte{0x00, 0x00, 0x01, streamID, 0x00, 0x00, 0x80, flags, byte(hlen)}
	stamp := func(v int64) []byte {
		u := uint64(v)
		return []byte{
			byte(0x21 | (u>>29)&0x0e),
			byte(u >> 22),
			byte(0x01 | (u>>14)&0xfe),
			byte(u >> 7),
			byte(0x01 | (u<<1)&0xfe),
		}
	}
	h = append(h, stamp(pts)...)
	if hasDTS {
		h = append(h, stamp(dts)...)
	}
	return h
}

func nalPayload(nalType byte, size int) []byte {
	b := []byte{0x00, 0x00, 0x00, 0x01, nalType}
	for len(b) < size {
		b = append(b, byte(len(b)))
	}
	return b
}
