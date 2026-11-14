package fixture

import (
	"tsprobe/internal/psi"
	"tsprobe/internal/tspkt"
)

// appendCRC finalizes a section: sets section_length and appends the CRC32.
func appendCRC(sec []byte) []byte {
	sl := len(sec) - 3 + 4
	sec[1] |= byte(sl >> 8)
	sec[2] = byte(sl)
	crc := psi.CRC32MPEG(sec)
	return append(sec, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
}

// buildPAT builds a PAT section with a NIT entry and all programs.
func buildPAT(version, programs int) []byte {
	sec := []byte{
		0x00, 0xB0, 0x00,
		0x00, 0x01, // transport_stream_id
		0xC1 | byte(version<<1), // syntax + current_next
		0x00, 0x00,              // section 0 of 0
		0x00, 0x00, 0xE0, 0x10, // program 0 -> NIT PID 0x10
	}
	for p := 0; p < programs; p++ {
		prog := ProgramNumber(p)
		pid := PMTPID(p)
		sec = append(sec, byte(prog>>8), byte(prog), 0xE0|byte(pid>>8), byte(pid))
	}
	return appendCRC(sec)
}

// appendES adds one ES loop entry to a PMT section body.
func appendES(sec []byte, streamType byte, pid uint16, desc []byte) []byte {
	sec = append(sec, streamType, 0xE0|byte(pid>>8), byte(pid))
	sec = append(sec, 0xF0|byte(len(desc)>>8), byte(len(desc)))
	return append(sec, desc...)
}

// buildPMT builds a PMT section for program index prog.
func buildPMT(prog, version int) []byte {
	pcr := VideoPID(prog)
	sec := []byte{
		0x02, 0xB0, 0x00,
		byte(ProgramNumber(prog) >> 8), byte(ProgramNumber(prog)),
		0xC1 | byte(version<<1),
		0x00, 0x00,
		0xE0 | byte(pcr>>8), byte(pcr),
	}
	progInfo := []byte{0x05, 0x04, 'T', 'S', 'P', 'O'} // registration descriptor
	sec = append(sec, 0xF0|byte(len(progInfo)>>8), byte(len(progInfo)))
	sec = append(sec, progInfo...)
	sec = appendES(sec, 0x1B, VideoPID(prog), nil)
	sec = appendES(sec, 0x04, AudioPID(prog, 0), []byte{0x0A, 0x04, 'c', 'h', 'i', 0x00})
	sec = appendES(sec, 0x81, AudioPID(prog, 1), []byte{0x6A, 0x01, 0x00})
	return appendCRC(sec)
}

// psiPacket wraps one PSI section into a single TS packet.
func psiPacket(pid uint16, ccVal byte, section []byte) []byte {
	pkt := make([]byte, tspkt.PacketSize)
	pkt[0] = tspkt.SyncByte
	pkt[1] = 0x40 | byte(pid>>8)&0x1F
	pkt[2] = byte(pid)
	pkt[3] = 0x10 | (ccVal & 0x0F)
	pkt[4] = 0x00
	n := copy(pkt[5:], section)
	for i := 5 + n; i < len(pkt); i++ {
		pkt[i] = 0xFF
	}
	return pkt
}
