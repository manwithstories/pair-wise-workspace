package synth

import (
	"encoding/binary"

	"tsaudit/internal/psi"
)

type streamDef struct {
	Program     uint16
	PID         uint16
	StreamType  uint8
	StreamID    uint8
	Description string
}

type programDef struct {
	Number  uint16
	PMTPID  uint16
	PCRPID  uint16
	Streams []streamDef
}

func makePrograms(count int) []programDef {
	programs := make([]programDef, 0, count)
	for i := 0; i < count; i++ {
		program := uint16(i + 1)
		pmtPID := uint16(0x100 + i)
		base := uint16(0x200 + i*0x10)
		streamCount := 3
		if i == 0 {
			streamCount = 4
		}
		p := programDef{Number: program, PMTPID: pmtPID, PCRPID: base}
		for j := 0; j < streamCount; j++ {
			p.Streams = append(p.Streams, streamDef{
				Program: program, PID: base + uint16(j), StreamType: streamTypeFor(program, j),
				StreamID: streamIDFor(j), Description: "synthetic elementary stream",
			})
		}
		programs = append(programs, p)
	}
	return programs
}

func streamTypeFor(program uint16, index int) uint8 {
	switch index {
	case 0:
		return 0x1b
	case 1:
		return 0x0f
	case 2:
		if program == 3 {
			return 0x06
		}
		return 0x0f
	default:
		return 0x15
	}
}

func streamIDFor(index int) uint8 {
	switch index {
	case 0:
		return 0xe0
	case 1:
		return 0xc0
	case 2:
		return 0xc1
	default:
		return 0xbd
	}
}

func (s streamDef) isVideo() bool { return s.StreamType == 0x1b }

func buildPAT(programs []programDef, version uint8) [][]byte {
	entries := make([]byte, 0, len(programs)*4)
	for _, p := range programs {
		var b [4]byte
		binary.BigEndian.PutUint16(b[0:2], p.Number)
		binary.BigEndian.PutUint16(b[2:4], 0xe000|p.PMTPID)
		entries = append(entries, b[:]...)
	}
	if len(programs) <= 1 {
		return [][]byte{buildSection(psi.TablePAT, 1, version, 0, 0, entries)}
	}
	split := (len(programs) + 1) / 2
	out := make([][]byte, 0, 2)
	offsets := []int{0, split * 4, len(entries)}
	for i := 0; i < 2; i++ {
		out = append(out, buildSection(psi.TablePAT, 1, version, uint8(i), 1, entries[offsets[i]:offsets[i+1]]))
	}
	return out
}

func buildPMT(p programDef, version, revision uint8) []byte {
	desc := []byte{0x05, 0x04, 'T', 'S', 'A', 'U', 0x66, 0x02, revision, version}
	body := make([]byte, 0, 64)
	var b [4]byte
	binary.BigEndian.PutUint16(b[0:2], 0xe000|p.PCRPID)
	binary.BigEndian.PutUint16(b[2:4], 0xf000|uint16(len(desc)))
	body = append(body, b[:]...)
	body = append(body, desc...)
	for _, es := range p.Streams {
		esDesc := descriptorFor(es)
		body = append(body, es.StreamType)
		var head [4]byte
		binary.BigEndian.PutUint16(head[0:2], 0xe000|es.PID)
		binary.BigEndian.PutUint16(head[2:4], 0xf000|uint16(len(esDesc)))
		body = append(body, head[:]...)
		body = append(body, esDesc...)
	}
	return buildSection(psi.TablePMT, p.Number, version, 0, 0, body)
}

func descriptorFor(es streamDef) []byte {
	switch {
	case es.StreamType == 0x1b:
		return []byte{0x28, 0x04, 0x64, 0x00, 0x1f, 0xe1}
	case es.StreamType == 0x06:
		return []byte{0x59, 0x08, 'e', 'n', 'g', 0x00, 0x01, 0x00, 0x02, 0x00}
	case es.StreamType == 0x15:
		return []byte{0x66, 0x02, 0x01, 0x02}
	default:
		return []byte{0x0a, 0x04, 'e', 'n', 'g', 0x00}
	}
}

func buildSection(tableID uint8, extension uint16, version, number, last uint8, body []byte) []byte {
	length := 5 + len(body) + 4
	out := make([]byte, 3+length)
	out[0] = tableID
	out[1] = 0xb0 | byte(length>>8)
	out[2] = byte(length)
	binary.BigEndian.PutUint16(out[3:5], extension)
	out[5] = 0xc0 | (version&0x1f)<<1 | 1
	out[6] = number
	out[7] = last
	copy(out[8:], body)
	crc := psi.CRC32MPEG(out[:len(out)-4])
	binary.BigEndian.PutUint32(out[len(out)-4:], crc)
	return out
}
