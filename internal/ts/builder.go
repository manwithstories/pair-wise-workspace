package ts

// BuildPAT builds a single-program PAT section including CRC.
func BuildPAT(version byte, pmtPID uint16) []byte {
	body := []byte{
		0x00,       // table_id
		0xb0, 0x0d, // section_syntax=1, length=13
		0x00, 0x01, // transport_stream_id
		0xc1 | (version&0x1f)<<1, // reserved + version + current_next
		0x00, 0x00,               // section/last
		0x00, 0x01, // program_number 1
		0xe0 | byte(pmtPID>>8)&0x1f, byte(pmtPID),
	}
	return WithCRC(body)
}

// BuildPMT builds a PMT with one AVC video PID and one AAC audio PID.
func BuildPMT(version byte, pcrPID, videoPID, audioPID uint16) []byte {
	// section_length = 9 fixed bytes after length + two 5-byte entries + CRC4.
	body := []byte{
		0x02,
		0xb0, 0x17, // section_length 23
		0x00, 0x01, // program number
		0xc1 | (version&0x1f)<<1,
		0x00, 0x00,
		0xe0 | byte(pcrPID>>8)&0x1f, byte(pcrPID),
		0xf0, 0x00, // program info length 0
		0x1b, // AVC video
		0xe0 | byte(videoPID>>8)&0x1f, byte(videoPID),
		0xf0, 0x00,
		0x0f, // AAC ADTS audio
		0xe0 | byte(audioPID>>8)&0x1f, byte(audioPID),
		0xf0, 0x00,
	}
	return WithCRC(body)
}

// PacketizeSection puts a PSI section into one or more TS payload units.
// The returned packets have sync bytes and continuity counters initialized.
func PacketizeSection(pid uint16, section []byte, startCC byte) [][]byte {
	var out [][]byte
	cc := startCC & 0x0f
	first := true
	for len(section) > 0 || first {
		pkt := make([]byte, PacketSize)
		pkt[0] = SyncByte
		pkt[1] = byte(pid>>8) & 0x1f
		pkt[2] = byte(pid)
		pkt[3] = 0x10 | cc // payload only
		pos := 4
		if first {
			pkt[1] |= 0x40 // PUSI
			pkt[4] = 0x00  // pointer field
			pos = 5
			first = false
		}
		n := copy(pkt[pos:], section)
		section = section[n:]
		out = append(out, pkt)
		cc = (cc + 1) & 0x0f
	}
	return out
}

// BuildMediaPacket is a deterministic payload-only TS packet. Adaptation may
// carry PCR and random-access marking.
func BuildMediaPacket(pid uint16, cc byte, pusi bool, payload []byte, pcr27 *uint64, randomAccess bool, discontinuity bool) []byte {
	pkt := make([]byte, PacketSize)
	pkt[0] = SyncByte
	pkt[1] = byte(pid>>8) & 0x1f
	if pusi {
		pkt[1] |= 0x40
	}
	pkt[2] = byte(pid)
	afc := byte(0x01) // payload only
	pos := 4
	if pcr27 != nil || randomAccess || discontinuity {
		afc = 0x03 // adaptation + payload
		flags := byte(0)
		length := 1
		if discontinuity {
			flags |= 0x80
		}
		if randomAccess {
			flags |= 0x40
		}
		if pcr27 != nil {
			flags |= 0x10
			length += 6
		}
		pkt[4] = byte(length)
		pkt[5] = flags
		if pcr27 != nil {
			base := *pcr27 / 300
			ext := *pcr27 % 300
			pcr := make([]byte, 6)
			pcr[0] = byte(base >> 25)
			pcr[1] = byte(base >> 17)
			pcr[2] = byte(base >> 9)
			pcr[3] = byte(base >> 1)
			pcr[4] = byte(base<<7) | 0x7e | byte((ext>>8)&0x01)
			pcr[5] = byte(ext)
			copy(pkt[6:12], pcr)
		}
		pos = 6 + int(pkt[4]) - 1 // 5 + length? adaptation starts pkt[4] length byte; content pkt[5], total 1+length
		// Explicitly: next byte after length byte plus length bytes.
		pos = 5 + int(pkt[4])
	}
	pkt[3] = (afc << 4) | (cc & 0x0f)
	copy(pkt[pos:], payload)
	return pkt
}

// EncodePTS stores a 33-bit PTS in the five-byte MPEG timestamp layout. Values
// at or beyond the wrap modulus are encoded as their wrapped raw value; the
// media clock/index expander restores monotonic time.
func EncodePTS(pts90k uint64) []byte {
	pts90k &= (1 << 33) - 1
	var b [5]byte
	b[0] = 0x21 | byte((pts90k>>29)&0x0e)
	b[1] = byte((pts90k >> 22) & 0xff)
	b[2] = byte((pts90k>>14)&0xfe) | 0x01
	b[3] = byte((pts90k >> 7) & 0xff)
	b[4] = byte((pts90k<<1)&0xfe) | 0x01
	return b[:]
}
