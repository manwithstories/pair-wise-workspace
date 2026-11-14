package psi

import (
	"encoding/binary"
	"fmt"
)

// ElementaryStream is one ES loop entry.
type ElementaryStream struct {
	StreamType   uint8        `json:"stream_type"`
	PID          uint16       `json:"pid"`
	InfoLength   int          `json:"info_length"`
	Descriptors  []Descriptor `json:"descriptors,omitempty"`
	Language     string       `json:"language,omitempty"`
	Registration []string     `json:"registration,omitempty"`
}

// PMT is a parsed program map table.
type PMT struct {
	ProgramNumber     uint16             `json:"program"`
	Version           uint8              `json:"version"`
	CurrentNext       bool               `json:"current_next"`
	SectionNumber     uint8              `json:"section_number"`
	LastSectionNumber uint8              `json:"last_section_number"`
	PCRPID            uint16             `json:"pcr_pid"`
	ProgramInfo       []Descriptor       `json:"program_info,omitempty"`
	Streams           []ElementaryStream `json:"streams"`
	StartPacket       int64              `json:"start_pkt"`
}

// ParsePMT parses a CRC-validated PMT section body.
func ParsePMT(s *Section) (*PMT, error) {
	if s.TableID != TableIDPMT {
		return nil, fmt.Errorf("psi: table 0x%02x is not PMT", s.TableID)
	}
	if len(s.Body) < 4 {
		return nil, fmt.Errorf("psi: PMT body too short")
	}
	p := &PMT{
		ProgramNumber:     s.TableIDExtension,
		Version:           s.Version,
		CurrentNext:       s.CurrentNext,
		SectionNumber:     s.SectionNumber,
		LastSectionNumber: s.LastSectionNumber,
		PCRPID:            binary.BigEndian.Uint16(s.Body[0:2]) & 0x1fff,
		StartPacket:       s.StartPacket,
	}
	infoLen := int(binary.BigEndian.Uint16(s.Body[2:4]) & 0x0fff)
	if 4+infoLen > len(s.Body) {
		return nil, fmt.Errorf("psi: program_info_length exceeds PMT")
	}
	var err error
	p.ProgramInfo, err = ParseDescriptors(s.Body[4 : 4+infoLen])
	if err != nil {
		return nil, err
	}
	pos := 4 + infoLen
	for pos < len(s.Body) {
		if pos+5 > len(s.Body) {
			return nil, fmt.Errorf("psi: truncated ES loop")
		}
		streamType := s.Body[pos]
		pid := binary.BigEndian.Uint16(s.Body[pos+1:pos+3]) & 0x1fff
		n := int(binary.BigEndian.Uint16(s.Body[pos+3:pos+5]) & 0x0fff)
		if pos+5+n > len(s.Body) {
			return nil, fmt.Errorf("psi: ES_info_length exceeds PMT")
		}
		descs, derr := ParseDescriptors(s.Body[pos+5 : pos+5+n])
		if derr != nil {
			return nil, derr
		}
		es := ElementaryStream{
			StreamType:   streamType,
			PID:          pid,
			InfoLength:   n,
			Descriptors:  descs,
			Language:     firstLanguage(descs),
			Registration: Registrations(descs),
		}
		p.Streams = append(p.Streams, es)
		pos += 5 + n
	}
	return p, nil
}

func firstLanguage(ds []Descriptor) string {
	for _, d := range ds {
		if d.Language != "" {
			return d.Language
		}
	}
	return ""
}

// PMTBuildStream is one ES loop entry to serialize.
type PMTBuildStream struct {
	StreamType uint8
	PID        uint16
	ESInfo     []byte
}

// BuildPMT serializes a single-section PMT with a valid MPEG-2 CRC.
func BuildPMT(program uint16, pcrPID uint16, version uint8, current bool, programInfo []byte, streams []PMTBuildStream) []byte {
	s := make([]byte, 12)
	s[0] = TableIDPMT
	s[1] = 0xb0
	binary.BigEndian.PutUint16(s[3:5], program)
	s[5] = 0xc0 | (version&0x1f)<<1
	if current {
		s[5] |= 1
	}
	binary.BigEndian.PutUint16(s[8:10], 0xe000|pcrPID&0x1fff)
	binary.BigEndian.PutUint16(s[10:12], uint16(len(programInfo))&0x0fff)
	s = append(s, programInfo...)
	for _, es := range streams {
		s = append(s, es.StreamType)
		var hdr [4]byte
		binary.BigEndian.PutUint16(hdr[0:2], 0xe000|es.PID&0x1fff)
		binary.BigEndian.PutUint16(hdr[2:4], uint16(len(es.ESInfo))&0x0fff)
		s = append(s, hdr[:]...)
		s = append(s, es.ESInfo...)
	}
	return FinalizeSection(s)
}
