package psi

import (
	"encoding/binary"
	"fmt"
)

// PATProgram is one program entry in a PAT.
type PATProgram struct {
	ProgramNumber uint16 `json:"program"`
	PID           uint16 `json:"pid"`
	IsNIT         bool   `json:"nit,omitempty"`
}

// PAT is a parsed program association table.
type PAT struct {
	TransportStreamID uint16       `json:"transport_stream_id"`
	Version           uint8        `json:"version"`
	CurrentNext       bool         `json:"current_next"`
	SectionNumber     uint8        `json:"section_number"`
	LastSectionNumber uint8        `json:"last_section_number"`
	Programs          []PATProgram `json:"programs"`
	StartPacket       int64        `json:"start_pkt"`
}

// ParsePAT parses a CRC-validated PAT section body.
func ParsePAT(s *Section) (*PAT, error) {
	if s.TableID != TableIDPAT {
		return nil, fmt.Errorf("psi: table 0x%02x is not PAT", s.TableID)
	}
	if len(s.Body)%4 != 0 {
		return nil, fmt.Errorf("psi: PAT body length %d is not a multiple of 4", len(s.Body))
	}
	p := &PAT{
		TransportStreamID: s.TableIDExtension,
		Version:           s.Version,
		CurrentNext:       s.CurrentNext,
		SectionNumber:     s.SectionNumber,
		LastSectionNumber: s.LastSectionNumber,
		StartPacket:       s.StartPacket,
	}
	for i := 0; i+4 <= len(s.Body); i += 4 {
		program := binary.BigEndian.Uint16(s.Body[i : i+2])
		pid := binary.BigEndian.Uint16(s.Body[i+2:i+4]) & 0x1fff
		p.Programs = append(p.Programs, PATProgram{ProgramNumber: program, PID: pid, IsNIT: program == 0})
	}
	return p, nil
}

// PATBuildProgram is one program to serialize into a PAT.
type PATBuildProgram struct {
	ProgramNumber uint16
	PID           uint16
}

// BuildPAT serializes a single-section PAT with a valid MPEG-2 CRC.
func BuildPAT(tsid uint16, version uint8, current bool, programs []PATBuildProgram) []byte {
	s := make([]byte, 8, 8+4*len(programs)+4)
	s[0] = TableIDPAT
	s[1] = 0xb0
	binary.BigEndian.PutUint16(s[3:5], tsid)
	s[5] = 0xc0 | (version&0x1f)<<1
	if current {
		s[5] |= 1
	}
	for _, p := range programs {
		var entry [4]byte
		binary.BigEndian.PutUint16(entry[0:2], p.ProgramNumber)
		binary.BigEndian.PutUint16(entry[2:4], 0xe000|p.PID&0x1fff)
		s = append(s, entry[:]...)
	}
	return FinalizeSection(s)
}
