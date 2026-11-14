package psi

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// PAT is a complete, merged Program Association Table.
type PAT struct {
	TransportStreamID uint16
	Version           uint8
	Programs          map[uint16]uint16
	ContentHash       [32]byte
}

// PATMerger merges sections belonging to one PAT version.
type PATMerger struct {
	tsid     uint16
	version  uint8
	last     uint8
	sections map[uint8]map[uint16]uint16
}

func (m *PATMerger) Add(sec Section) (*PAT, bool, error) {
	part, err := ParsePAT(sec)
	if err != nil {
		return nil, false, err
	}
	if m.sections == nil || m.version != part.Version || m.tsid != part.TransportStreamID {
		m.tsid = part.TransportStreamID
		m.version = part.Version
		m.last = part.LastSectionNumber
		m.sections = make(map[uint8]map[uint16]uint16)
	}
	if part.LastSectionNumber != m.last || int(part.SectionNumber) > int(m.last) {
		return nil, false, errors.New("psi: inconsistent PAT section numbering")
	}
	m.sections[part.SectionNumber] = part.Programs
	if len(m.sections) != int(m.last)+1 {
		return nil, false, nil
	}
	programs := make(map[uint16]uint16)
	for i := 0; i <= int(m.last); i++ {
		part, ok := m.sections[uint8(i)]
		if !ok {
			return nil, false, nil
		}
		for program, pid := range part {
			programs[program] = pid
		}
	}
	out := &PAT{
		TransportStreamID: m.tsid,
		Version:           m.version,
		Programs:          programs,
	}
	out.ContentHash = patHash(out)
	return out, true, nil
}

// PATSection is one parsed PAT section before multi-section merging.
type PATSection struct {
	TransportStreamID uint16
	Version           uint8
	CurrentNext       bool
	SectionNumber     uint8
	LastSectionNumber uint8
	Programs          map[uint16]uint16
}

func ParsePAT(sec Section) (*PATSection, error) {
	if sec.TableID != TablePAT {
		return nil, fmt.Errorf("psi: expected PAT, got table 0x%02x", sec.TableID)
	}
	if len(sec.Body)%4 != 0 {
		return nil, errors.New("psi: PAT body length is not a multiple of four")
	}
	programs := make(map[uint16]uint16)
	for i := 0; i+4 <= len(sec.Body); i += 4 {
		program := binary.BigEndian.Uint16(sec.Body[i : i+2])
		pid := binary.BigEndian.Uint16(sec.Body[i+2:i+4]) & 0x1fff
		if program == 0 {
			continue // NIT PID is intentionally ignored.
		}
		if pid == 0x1fff || pid == 0 {
			return nil, fmt.Errorf("psi: invalid PMT PID 0x%04x for program %d", pid, program)
		}
		programs[program] = pid
	}
	return &PATSection{
		TransportStreamID: sec.TableIDExtension,
		Version:           sec.Version,
		CurrentNext:       sec.CurrentNext,
		SectionNumber:     sec.SectionNumber,
		LastSectionNumber: sec.LastSectionNumber,
		Programs:          programs,
	}, nil
}

func patHash(p *PAT) [32]byte {
	programs := make([]int, 0, len(p.Programs))
	for program := range p.Programs {
		programs = append(programs, int(program))
	}
	sort.Ints(programs)
	buf := make([]byte, 2+2+len(programs)*4)
	binary.BigEndian.PutUint16(buf[0:2], p.TransportStreamID)
	binary.BigEndian.PutUint16(buf[2:4], uint16(len(programs)))
	for i, program := range programs {
		off := 4 + i*4
		binary.BigEndian.PutUint16(buf[off:off+2], uint16(program))
		binary.BigEndian.PutUint16(buf[off+2:off+4], p.Programs[uint16(program)])
	}
	return sha256.Sum256(buf)
}
