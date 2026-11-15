package fixture

import (
	"encoding/binary"

	"tsprobe/internal/psi"
)

// TransportStreamID is the synthetic transport stream id used by all fixtures.
const TransportStreamID uint16 = 1

type programConfig struct {
	number    uint16
	pmtPID    uint16
	videoPID  uint16
	audioPID  uint16
	videoType uint8
	audioType uint8
}

func defaultPrograms(n int) []programConfig {
	programs := make([]programConfig, 0, n)
	for i := 0; i < n; i++ {
		programs = append(programs, programConfig{
			number:    uint16(i + 1),
			pmtPID:    uint16(0x1000 + i),
			videoPID:  uint16(0x0101 + i*0x20),
			audioPID:  uint16(0x0102 + i*0x20),
			videoType: 0x1b,
			audioType: 0x0f,
		})
	}
	return programs
}

func buildPAT(version uint8, currentNext bool, programs []programConfig) []byte {
	body := make([]byte, 0, 2+len(programs)*4)
	body = binary.BigEndian.AppendUint16(body, TransportStreamID)
	for _, p := range programs {
		body = binary.BigEndian.AppendUint16(body, p.number)
		body = binary.BigEndian.AppendUint16(body, 0xe000|p.pmtPID)
	}
	return longSection(psi.TableIDPAT, TransportStreamID, version, currentNext, body)
}

func buildPMT(p programConfig, version uint8, currentNext bool) []byte {
	body := make([]byte, 0, 4+len(defaultStreamEntries(p)))
	body = binary.BigEndian.AppendUint16(body, 0xe000|p.videoPID)
	body = binary.BigEndian.AppendUint16(body, 0xf000)
	for _, es := range defaultStreamEntries(p) {
		body = append(body, es.streamType)
		body = binary.BigEndian.AppendUint16(body, 0xe000|es.pid)
		body = binary.BigEndian.AppendUint16(body, 0xf000)
	}
	return longSection(psi.TableIDPMT, p.number, version, currentNext, body)
}

type streamEntry struct {
	streamType uint8
	pid        uint16
}

func defaultStreamEntries(p programConfig) []streamEntry {
	return []streamEntry{{p.videoType, p.videoPID}, {p.audioType, p.audioPID}}
}

func buildSDT(version uint8, currentNext bool, programs []programConfig) []byte {
	body := binary.BigEndian.AppendUint16(nil, TransportStreamID)
	body = append(body, 0xff)
	for _, p := range programs {
		body = binary.BigEndian.AppendUint16(body, p.number)
		body = append(body, 0x80)
		desc := serviceDescriptor("tsprobe", "Program "+itoa(int(p.number)))
		body = binary.BigEndian.AppendUint16(body, 0xf000|uint16(len(desc)))
		body = append(body, desc...)
	}
	return longSection(psi.TableIDSDT, TransportStreamID, version, currentNext, body)
}

func longSection(tableID uint8, extension uint16, version uint8, currentNext bool, body []byte) []byte {
	sectionLength := 5 + len(body) + 4
	raw := []byte{tableID, 0xb0 | byte(sectionLength>>8), byte(sectionLength)}
	raw = binary.BigEndian.AppendUint16(raw, extension)
	cn := byte(0)
	if currentNext {
		cn = 1
	}
	raw = append(raw, 0xc0|(version&0x1f)<<1|cn, 0, 0)
	raw = append(raw, body...)
	crc := psi.CRC32MPEG(raw)
	raw = binary.BigEndian.AppendUint32(raw, crc)
	return raw
}

func serviceDescriptor(provider, name string) []byte {
	inner := []byte{byte(len(provider))}
	inner = append(inner, provider...)
	inner = append(inner, byte(len(name)))
	inner = append(inner, name...)
	return append([]byte{0x48, byte(len(inner))}, inner...)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
