package ts

import "encoding/binary"

func makeSection(tableID uint8, ext uint16, version uint8, body []byte) []byte {
	length := len(body) + 5 + 4
	sec := make([]byte, 3+length)
	sec[0] = tableID
	sec[1] = 0xB0 | byte(length>>8&0x0f)
	sec[2] = byte(length)
	binary.BigEndian.PutUint16(sec[3:], ext)
	sec[5] = 0xC1 | version<<1&0x1e // current_next=1
	sec[6] = 0
	sec[7] = 0
	copy(sec[8:], body)
	binary.BigEndian.PutUint32(sec[len(sec)-4:], CRC32(sec[:len(sec)-4]))
	return sec
}

func patSection(pmtPID uint16, version uint8) []byte {
	body := []byte{0x00, 0x01, byte(0xE0 | pmtPID>>8), byte(pmtPID)}
	return makeSection(0x00, 0x0001, version, body)
}

func pmtSection(pcrPID uint16, version uint8, streams ...[2]uint16) []byte {
	body := []byte{byte(0xE0 | pcrPID>>8), byte(pcrPID), 0xF0, 0x00}
	for _, s := range streams {
		body = append(body, byte(s[0]), byte(0xE0|s[1]>>8), byte(s[1]), 0xF0, 0x00)
	}
	return makeSection(0x02, 0x0001, version, body)
}

func sectionPackets(pid uint16, sec []byte, cc *uint8) [][]byte {
	// Single-packet PSI with a zero pointer field; sections here fit.
	payload := append([]byte{0x00}, sec...)
	p := tsPacket(pid, *cc, true, payload, 0, false)
	*cc++
	return [][]byte{p}
}
