package psi

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	TablePAT = 0x00
	TablePMT = 0x02
	TableSDT = 0x42
)

var ErrShortSection = errors.New("psi: short section")

type Header struct {
	TableID          byte
	SectionSyntax    bool
	SectionLength    int
	TableIDExtension uint16
	Version          uint8
	CurrentNext      bool
	SectionNumber    uint8
	LastSection      uint8
}

type PATProgram struct {
	Program uint16
	PID     uint16
}

type PAT struct {
	Header
	TransportStreamID uint16
	Programs          []PATProgram
}

type Descriptor struct {
	Tag  byte
	Data []byte
}

type ESInfo struct {
	StreamType    byte
	ElementaryPID uint16
	Descriptors   []Descriptor
}

type PMT struct {
	Header
	Program     uint16
	PCRPID      uint16
	Descriptors []Descriptor
	Streams     []ESInfo
}

type Service struct {
	ID          uint16
	ServiceType byte
	Descriptors []Descriptor
}

type SDT struct {
	Header
	TransportStreamID uint16
	OriginalNetworkID uint16
	Services          []Service
}

func parseHeader(data []byte, tableID byte) (Header, error) {
	if len(data) < 12 {
		return Header{}, ErrShortSection
	}
	if data[0] != tableID {
		return Header{}, fmt.Errorf("psi: unexpected table_id %#x, want %#x", data[0], tableID)
	}
	if data[1]&0x80 == 0 {
		return Header{}, errors.New("psi: section_syntax_indicator is not set")
	}
	length := sectionLength(data[1:3])
	if length < MinSectionLength || length > MaxSectionLength || 3+length != len(data) {
		return Header{}, fmt.Errorf("psi: section_length %d does not match %d-byte section", length, len(data))
	}
	return Header{
		TableID:          tableID,
		SectionSyntax:    true,
		SectionLength:    length,
		TableIDExtension: binary.BigEndian.Uint16(data[3:5]),
		Version:          (data[5] >> 1) & 0x1f,
		CurrentNext:      data[5]&1 == 1,
		SectionNumber:    data[6],
		LastSection:      data[7],
	}, nil
}

func ParsePAT(data []byte) (PAT, error) {
	h, err := parseHeader(data, TablePAT)
	if err != nil {
		return PAT{}, err
	}
	if (len(data)-8)%4 != 0 {
		return PAT{}, errors.New("psi: PAT program loop is misaligned")
	}
	out := PAT{Header: h, TransportStreamID: h.TableIDExtension}
	for pos := 8; pos < len(data)-4; pos += 4 {
		out.Programs = append(out.Programs, PATProgram{
			Program: binary.BigEndian.Uint16(data[pos : pos+2]),
			PID:     binary.BigEndian.Uint16(data[pos+2:pos+4]) & 0x1fff,
		})
	}
	return out, nil
}

func parseDescriptors(data []byte) ([]Descriptor, error) {
	var out []Descriptor
	for len(data) > 0 {
		if len(data) < 2 || int(data[1]) > len(data)-2 {
			return nil, errors.New("psi: malformed descriptor loop")
		}
		out = append(out, Descriptor{Tag: data[0], Data: append([]byte(nil), data[2:2+int(data[1])]...)})
		data = data[2+int(data[1]):]
	}
	return out, nil
}

func ParsePMT(data []byte) (PMT, error) {
	h, err := parseHeader(data, TablePMT)
	if err != nil {
		return PMT{}, err
	}
	if len(data) < 16 {
		return PMT{}, ErrShortSection
	}
	infoLen := int(binary.BigEndian.Uint16(data[10:12]) & 0x0fff)
	if 12+infoLen > len(data)-4 {
		return PMT{}, errors.New("psi: PMT program_info_length out of range")
	}
	info, err := parseDescriptors(data[12 : 12+infoLen])
	if err != nil {
		return PMT{}, err
	}
	out := PMT{
		Header:      h,
		Program:     h.TableIDExtension,
		PCRPID:      binary.BigEndian.Uint16(data[8:10]) & 0x1fff,
		Descriptors: info,
	}
	pos := 12 + infoLen
	end := len(data) - 4
	for pos < end {
		if pos+5 > end {
			return PMT{}, errors.New("psi: truncated PMT ES loop")
		}
		esLen := int(binary.BigEndian.Uint16(data[pos+3:pos+5]) & 0x0fff)
		if pos+5+esLen > end {
			return PMT{}, errors.New("psi: PMT ES_info_length out of range")
		}
		desc, err := parseDescriptors(data[pos+5 : pos+5+esLen])
		if err != nil {
			return PMT{}, err
		}
		out.Streams = append(out.Streams, ESInfo{
			StreamType:    data[pos],
			ElementaryPID: binary.BigEndian.Uint16(data[pos+1:pos+3]) & 0x1fff,
			Descriptors:   desc,
		})
		pos += 5 + esLen
	}
	if pos != end {
		return PMT{}, errors.New("psi: PMT ES loop is misaligned")
	}
	return out, nil
}

func ParseSDT(data []byte) (SDT, error) {
	h, err := parseHeader(data, TableSDT)
	if err != nil {
		return SDT{}, err
	}
	if len(data) < 15 {
		return SDT{}, ErrShortSection
	}
	out := SDT{
		Header:            h,
		TransportStreamID: h.TableIDExtension,
		OriginalNetworkID: binary.BigEndian.Uint16(data[8:10]),
	}
	pos, end := 11, len(data)-4
	for pos < end {
		if pos+5 > end {
			return SDT{}, errors.New("psi: truncated SDT service loop")
		}
		descLen := int(binary.BigEndian.Uint16(data[pos+3:pos+5]) & 0x0fff)
		if pos+5+descLen > end {
			return SDT{}, errors.New("psi: SDT descriptor length out of range")
		}
		desc, err := parseDescriptors(data[pos+5 : pos+5+descLen])
		if err != nil {
			return SDT{}, err
		}
		out.Services = append(out.Services, Service{
			ID:          binary.BigEndian.Uint16(data[pos : pos+2]),
			ServiceType: data[pos+2],
			Descriptors: desc,
		})
		pos += 5 + descLen
	}
	if pos != end {
		return SDT{}, errors.New("psi: SDT service loop is misaligned")
	}
	return out, nil
}

func StreamTypeName(streamType byte) string {
	switch streamType {
	case 0x01:
		return "MPEG1Video"
	case 0x02:
		return "MPEG2Video"
	case 0x03:
		return "MPEG1Audio"
	case 0x04:
		return "MPEG2Audio"
	case 0x06:
		return "Private"
	case 0x0f:
		return "AAC"
	case 0x11:
		return "LATM"
	case 0x1b:
		return "H264"
	case 0x24:
		return "HEVC"
	case 0x81:
		return "AC3"
	case 0x87:
		return "EAC3"
	default:
		return fmt.Sprintf("0x%02x", streamType)
	}
}

func IsVideoStreamType(streamType byte) bool {
	switch streamType {
	case 0x01, 0x02, 0x1b, 0x24, 0x12, 0x10, 0x21, 0x99:
		return true
	default:
		return false
	}
}
