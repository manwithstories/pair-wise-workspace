package psi

import (
	"encoding/binary"
	"errors"
)

func buildSection(tableID byte, extension uint16, version uint8, body []byte) ([]byte, error) {
	length := 5 + len(body) + 4
	if length > MaxSectionLength {
		return nil, errors.New("psi: section exceeds 4093 bytes")
	}
	out := make([]byte, 8, 3+length)
	out[0] = tableID
	out[1] = 0xb0 | byte(length>>8)
	out[2] = byte(length)
	binary.BigEndian.PutUint16(out[3:5], extension)
	out[5] = 0xc0 | (version&0x1f)<<1 | 1
	out = append(out, body...)
	return AppendCRC(out), nil
}

func BuildPAT(transportStreamID uint16, version uint8, programs []PATProgram) ([]byte, error) {
	body := make([]byte, 0, len(programs)*4)
	for _, program := range programs {
		var entry [4]byte
		binary.BigEndian.PutUint16(entry[0:2], program.Program)
		binary.BigEndian.PutUint16(entry[2:4], 0xe000|(program.PID&0x1fff))
		body = append(body, entry[:]...)
	}
	return buildSection(TablePAT, transportStreamID, version, body)
}

func descriptorBytes(descriptors []Descriptor) []byte {
	var out []byte
	for _, desc := range descriptors {
		out = append(out, desc.Tag, byte(len(desc.Data)))
		out = append(out, desc.Data...)
	}
	return out
}

func BuildPMT(program, pcrPID uint16, version uint8, streams []ESInfo, descriptors []Descriptor) ([]byte, error) {
	programInfo := descriptorBytes(descriptors)
	if len(programInfo) > 0xfff {
		return nil, errors.New("psi: PMT descriptors too long")
	}
	body := make([]byte, 4)
	binary.BigEndian.PutUint16(body[0:2], 0xe000|(pcrPID&0x1fff))
	binary.BigEndian.PutUint16(body[2:4], uint16(len(programInfo)))
	body = append(body, programInfo...)
	for _, stream := range streams {
		info := descriptorBytes(stream.Descriptors)
		if len(info) > 0xfff {
			return nil, errors.New("psi: ES descriptors too long")
		}
		var entry [5]byte
		entry[0] = stream.StreamType
		binary.BigEndian.PutUint16(entry[1:3], 0xe000|(stream.ElementaryPID&0x1fff))
		binary.BigEndian.PutUint16(entry[3:5], uint16(len(info)))
		body = append(body, entry[:]...)
		body = append(body, info...)
	}
	return buildSection(TablePMT, program, version, body)
}

func BuildSDT(transportStreamID, originalNetworkID uint16, version uint8, services []Service) ([]byte, error) {
	body := make([]byte, 3)
	binary.BigEndian.PutUint16(body[0:2], originalNetworkID)
	body[2] = 0xff
	for _, service := range services {
		info := descriptorBytes(service.Descriptors)
		if len(info) > 0xfff {
			return nil, errors.New("psi: SDT descriptors too long")
		}
		var entry [5]byte
		binary.BigEndian.PutUint16(entry[0:2], service.ID)
		entry[2] = 0xfc
		entry[3] = 0x80 | byte(len(info)>>8)
		entry[4] = byte(len(info))
		body = append(body, entry[:]...)
		body = append(body, info...)
	}
	return buildSection(TableSDT, transportStreamID, version, body)
}

// PacketizeSection wraps one section into one or more 188-byte transport
// packets. The first packet carries pointer_field=0 and PUSI=1.
func PacketizeSection(pid uint16, cc uint8, section []byte) [][]byte {
	var packets [][]byte
	first := true
	for len(section) > 0 {
		capacity := 184
		if first {
			capacity = 183
		}
		n := len(section)
		if n > capacity {
			n = capacity
		}
		p := newPacket(pid, cc, first)
		payload := p[4:]
		if first {
			payload[0] = 0
			copy(payload[1:], section[:n])
		} else {
			copy(payload, section[:n])
		}
		packets = append(packets, p)
		section = section[n:]
		cc = (cc + 1) & 0x0f
		first = false
	}
	return packets
}

func newPacket(pid uint16, cc uint8, pusi bool) []byte {
	p := make([]byte, 188)
	for i := range p {
		p[i] = 0xff
	}
	p[0] = 0x47
	p[1] = byte(pid >> 8)
	if pusi {
		p[1] |= 0x40
	}
	p[2] = byte(pid)
	p[3] = 0x10 | (cc & 0x0f)
	return p
}
