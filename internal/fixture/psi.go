package fixture

import (
	"streamcut/internal/ts"
)

// BuildPATSection constructs a PAT section for one program and appends a
// correct CRC32-MPEG2, so parser and generator share one checksum definition.
func BuildPATSection(tsid uint16, version byte, pmtPID uint16) []byte {
	payload := make([]byte, 0, 13)
	payload = append(payload, byte(tsid>>8), byte(tsid))
	payload = append(payload, 0xC1|(version<<1), 0x00, 0x00)
	payload = append(payload, 0x00, 0x01) // program_number 1
	payload = append(payload, 0xE0|byte(pmtPID>>8), byte(pmtPID))
	return buildSection(0x00, payload)
}

// BuildPMTSection constructs a PMT referencing videoPID and audioPID. The
// audio type distinguishes the supported PCM private stream from an
// explicitly unsupported codec (e.g. AAC).
func BuildPMTSection(version byte, pcrPID, videoPID, audioPID uint16, audioType ts.StreamType) []byte {
	payload := make([]byte, 0, 22)
	payload = append(payload, 0x00, 0x01) // program number
	payload = append(payload, 0xC1|(version<<1), 0x00, 0x00)
	payload = append(payload, 0xE0|byte(pcrPID>>8), byte(pcrPID))
	payload = append(payload, 0xF0, 0x00) // no program descriptors
	payload = append(payload,
		byte(ts.STH264), 0xE0|byte(videoPID>>8), byte(videoPID), 0xF0, 0x00)
	payload = append(payload,
		byte(audioType), 0xE0|byte(audioPID>>8), byte(audioPID), 0xF0, 0x00)
	return buildSection(0x02, payload)
}

// buildSection wraps a table payload with the section header/length and CRC.
func buildSection(tableID byte, payload []byte) []byte {
	secLen := len(payload) + 4 // payload + CRC32
	body := []byte{
		tableID,
		0xB0 | byte(secLen>>8)&0x0F, // section_syntax_indicator=1
		byte(secLen),
	}
	body = append(body, payload...)
	crc := ts.CRC32MPEG2(body)
	return append(body, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
}

// CorruptSection flips payload bytes without recomputing the CRC, producing a
// syntactically present but checksum-invalid section for the rejection path.
func CorruptSection(section []byte) []byte {
	out := append([]byte(nil), section...)
	if len(out) > 12 {
		out[8] ^= 0xA5
		out[9] ^= 0x5A
	}
	return out
}
