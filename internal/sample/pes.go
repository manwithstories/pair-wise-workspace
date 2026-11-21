package sample

import "tsbatch/internal/ts"

// Well-known PIDs of generated streams.
const (
	PIDPAT   uint16 = 0x0000
	PIDPMT   uint16 = 0x1000
	PIDVideo uint16 = 0x0101
	PIDData  uint16 = 0x0102
)

// MediaPIDs returns the elementary-stream PIDs of generated streams.
func MediaPIDs() map[uint16]bool {
	return map[uint16]bool{PIDVideo: true, PIDData: true}
}

// buildPES builds one video PES packet: 14-byte header with PTS plus a
// deterministic ~400-byte payload. The first payload byte marks
// keyframes, the next four the frame number.
func buildPES(pcrBase uint64, frame int, keyframe bool) []byte {
	pts := (pcrBase + uint64(frame)*3600 + 1260) & ((1 << 33) - 1)
	pes := make([]byte, 0, 414)
	pes = append(pes, 0x00, 0x00, 0x01, 0xE0, 0x00, 0x00, 0x80, 0x80, 0x05)
	pes = append(pes,
		0x20|byte((pts>>30)&0x07)<<1|1,
		byte(pts>>22),
		byte(((pts>>15)&0x7F)<<1)|1,
		byte(pts>>7),
		byte((pts&0x7F)<<1)|1,
	)
	kf := byte(0)
	if keyframe {
		kf = 1
	}
	pes = append(pes, kf, byte(frame>>24), byte(frame>>16), byte(frame>>8), byte(frame))
	for i := len(pes); i < 414; i++ {
		pes = append(pes, byte(frame*7+i*3))
	}
	return pes
}

// buildSection assembles a PSI section with a valid CRC-32/MPEG.
func buildSection(tableID, version uint8, ext uint16, body []byte) []byte {
	secLen := 5 + len(body) + 4
	sec := make([]byte, 3+secLen)
	sec[0] = tableID
	sec[1] = 0xB0 | byte(secLen>>8)
	sec[2] = byte(secLen)
	sec[3] = byte(ext >> 8)
	sec[4] = byte(ext)
	sec[5] = 0xC1 | version<<1
	sec[6], sec[7] = 0, 0
	copy(sec[8:], body)
	crc := ts.CRC32MPEG(sec[:len(sec)-4])
	sec[len(sec)-4] = byte(crc >> 24)
	sec[len(sec)-3] = byte(crc >> 16)
	sec[len(sec)-2] = byte(crc >> 8)
	sec[len(sec)-1] = byte(crc)
	return sec
}

// patSection maps program 1 to the PMT PID.
func patSection(version uint8) []byte {
	return buildSection(0x00, version, 1, []byte{
		0x00, 0x01, byte(PIDPMT>>8) | 0xE0, byte(PIDPMT & 0xFF),
	})
}

// pmtSection lists the video and data elementary streams.
func pmtSection(version uint8) []byte {
	return buildSection(0x02, version, 1, []byte{
		byte(PIDVideo>>8) | 0xE0, byte(PIDVideo & 0xFF), // PCR PID
		0xF0, 0x00, // program info length
		0x1B, byte(PIDVideo>>8) | 0xE0, byte(PIDVideo & 0xFF), 0xF0, 0x00, // H.264 video
		0x06, byte(PIDData>>8) | 0xE0, byte(PIDData & 0xFF), 0xF0, 0x00, // private data (RTP)
	})
}

// PSIPacket builds a single-packet PSI table (PAT or PMT) with the
// given continuity counter. pid must be PIDPAT or PIDPMT.
func PSIPacket(pid uint16, version uint8, cc uint8) [ts.PacketSize]byte {
	var sec []byte
	if pid == PIDPAT {
		sec = patSection(version)
	} else {
		sec = pmtSection(version)
	}
	payload := append([]byte{0x00}, sec...) // pointer_field = 0
	var p [ts.PacketSize]byte
	p[0] = ts.SyncByte
	p[1] = 0x40 | byte(pid>>8)
	p[2] = byte(pid)
	afl := 183 - len(payload)
	if afl < 0 {
		p[3] = 0x10 | cc
		copy(p[4:], payload)
		return p
	}
	p[3] = 0x30 | cc
	p[4] = byte(afl)
	if afl >= 1 {
		p[5] = 0
		for j := 6; j < 5+afl; j++ {
			p[j] = 0xFF
		}
	}
	copy(p[5+afl:], payload)
	return p
}
