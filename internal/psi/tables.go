package psi

import (
	"fmt"
	"sort"
)

const (
	TablePAT = "PAT"
	TablePMT = "PMT"

	tableIDPAT = 0x00
	tableIDPMT = 0x02
)

type PATProgram struct {
	ProgramNumber uint16
	PMTPID        uint16
}

type PAT struct {
	TransportStreamID uint16
	Version           uint8
	CurrentNext       bool
	SectionNumber     uint8
	LastSection       uint8
	Programs          []PATProgram
}

type Stream struct {
	PID        uint16
	StreamType uint8
}

type PMT struct {
	ProgramNumber  uint16
	Version        uint8
	CurrentNext    bool
	SectionNumber  uint8
	LastSection    uint8
	PCRPID         uint16
	ProgramInfoLen uint16
	Streams        []Stream
}

type Change struct {
	Kind        string
	PID         uint16
	Version     uint8
	PacketIndex int64
	PAT         *PAT
	PMT         *PMT
}

type tableKey struct {
	tableID byte
	pid     uint16
	program uint16
}

type tableSet struct {
	version  uint8
	last     uint8
	sections map[uint8][]byte
}

type appliedTable struct {
	version  uint8
	sections map[uint8][]byte
}

type TableState struct {
	current map[tableKey]*tableSet
	staged  map[tableKey]*tableSet
	applied map[tableKey]*appliedTable
}

func NewTableState() *TableState {
	return &TableState{
		current: make(map[tableKey]*tableSet),
		staged:  make(map[tableKey]*tableSet),
		applied: make(map[tableKey]*appliedTable),
	}
}

func (s *TableState) Consume(sec Section) ([]Change, error) {
	if len(sec.Data) < 3 {
		return nil, fmt.Errorf("psi: pid %#x section is shorter than 3 bytes", sec.PID)
	}
	tableID := sec.Data[0]
	if tableID != tableIDPAT && tableID != tableIDPMT {
		return nil, nil
	}
	if len(sec.Data) < 8 {
		return nil, fmt.Errorf("psi: pid %#x table %#x header is truncated", sec.PID, tableID)
	}
	version := (sec.Data[5] >> 1) & 0x1f
	currentNext := sec.Data[5]&0x01 != 0
	sectionNumber := sec.Data[6]
	lastSection := sec.Data[7]
	if sectionNumber > lastSection {
		return nil, fmt.Errorf("psi: pid %#x section %d exceeds last_section_number %d", sec.PID, sectionNumber, lastSection)
	}
	var program uint16
	if tableID == tableIDPMT {
		program = uint16(sec.Data[3])<<8 | uint16(sec.Data[4])
	}
	key := tableKey{tableID: tableID, pid: sec.PID, program: program}
	if !currentNext {
		s.stage(key, version, lastSection, sectionNumber, sec.Data)
		return nil, nil
	}

	if a := s.applied[key]; a != nil && a.version == version {
		return nil, nil
	}
	set := s.current[key]
	if set == nil || set.version != version {
		set = &tableSet{version: version, last: lastSection, sections: make(map[uint8][]byte)}
		if preview := s.staged[key]; preview != nil && preview.version == version {
			for number, data := range preview.sections {
				set.sections[number] = append([]byte(nil), data...)
			}
		}
		s.current[key] = set
	}
	if set.last != lastSection {
		return nil, fmt.Errorf("psi: pid %#x inconsistent last_section_number", sec.PID)
	}
	if _, exists := set.sections[sectionNumber]; !exists {
		set.sections[sectionNumber] = append([]byte(nil), sec.Data...)
	}
	if len(set.sections) != int(lastSection)+1 {
		return nil, nil
	}

	change, sections, err := s.buildChange(key, set, sec.PacketIndex)
	if err != nil {
		return nil, err
	}
	s.applied[key] = &appliedTable{version: version, sections: sections}
	delete(s.current, key)
	delete(s.staged, key)
	return []Change{change}, nil
}

func (s *TableState) stage(key tableKey, version, last, number uint8, data []byte) {
	set := s.staged[key]
	if set == nil || set.version != version {
		set = &tableSet{version: version, last: last, sections: make(map[uint8][]byte)}
		s.staged[key] = set
	}
	if _, exists := set.sections[number]; !exists {
		set.sections[number] = append([]byte(nil), data...)
	}
}

func (s *TableState) buildChange(key tableKey, set *tableSet, packetIndex int64) (Change, map[uint8][]byte, error) {
	sections := make(map[uint8][]byte, len(set.sections))
	numbers := make([]int, 0, len(set.sections))
	for number := range set.sections {
		numbers = append(numbers, int(number))
	}
	sort.Ints(numbers)
	for _, number := range numbers {
		sections[uint8(number)] = append([]byte(nil), set.sections[uint8(number)]...)
	}
	if key.tableID == tableIDPAT {
		pat, err := mergePAT(sections)
		if err != nil {
			return Change{}, nil, err
		}
		return Change{Kind: TablePAT, PID: key.pid, Version: pat.Version, PacketIndex: packetIndex, PAT: pat}, sections, nil
	}
	pmt, err := mergePMT(sections)
	if err != nil {
		return Change{}, nil, err
	}
	return Change{Kind: TablePMT, PID: key.pid, Version: pmt.Version, PacketIndex: packetIndex, PMT: pmt}, sections, nil
}

func mergePAT(sections map[uint8][]byte) (*PAT, error) {
	var merged *PAT
	programs := make(map[uint16]uint16)
	for number := 0; number < len(sections); number++ {
		data, ok := sections[uint8(number)]
		if !ok {
			return nil, fmt.Errorf("psi: missing PAT section %d", number)
		}
		pat, err := ParsePAT(data)
		if err != nil {
			return nil, err
		}
		if merged == nil {
			merged = pat
		} else if merged.Version != pat.Version || merged.TransportStreamID != pat.TransportStreamID {
			return nil, fmt.Errorf("psi: inconsistent PAT sections")
		}
		for _, p := range pat.Programs {
			programs[p.ProgramNumber] = p.PMTPID
		}
	}
	if merged == nil {
		return nil, fmt.Errorf("psi: empty PAT")
	}
	merged.Programs = merged.Programs[:0]
	for number, pid := range programs {
		merged.Programs = append(merged.Programs, PATProgram{ProgramNumber: number, PMTPID: pid})
	}
	sort.Slice(merged.Programs, func(i, j int) bool {
		return merged.Programs[i].ProgramNumber < merged.Programs[j].ProgramNumber
	})
	return merged, nil
}

func mergePMT(sections map[uint8][]byte) (*PMT, error) {
	var merged *PMT
	seen := make(map[uint16]bool)
	for number := 0; number < len(sections); number++ {
		data, ok := sections[uint8(number)]
		if !ok {
			return nil, fmt.Errorf("psi: missing PMT section %d", number)
		}
		pmt, err := ParsePMT(data)
		if err != nil {
			return nil, err
		}
		if merged == nil {
			merged = pmt
			for _, stream := range pmt.Streams {
				seen[stream.PID] = true
			}
			continue
		} else if merged.Version != pmt.Version || merged.ProgramNumber != pmt.ProgramNumber || merged.PCRPID != pmt.PCRPID {
			return nil, fmt.Errorf("psi: inconsistent PMT sections")
		}
		for _, stream := range pmt.Streams {
			if !seen[stream.PID] {
				merged.Streams = append(merged.Streams, stream)
				seen[stream.PID] = true
			}
		}
	}
	if merged == nil {
		return nil, fmt.Errorf("psi: empty PMT")
	}
	sort.Slice(merged.Streams, func(i, j int) bool { return merged.Streams[i].PID < merged.Streams[j].PID })
	return merged, nil
}

func ParsePAT(data []byte) (*PAT, error) {
	if len(data) < 12 || data[0] != tableIDPAT {
		return nil, fmt.Errorf("psi: malformed PAT")
	}
	if err := checkSectionLength(data); err != nil {
		return nil, err
	}
	bodyEnd := len(data) - 4
	if (bodyEnd-8)%4 != 0 {
		return nil, fmt.Errorf("psi: PAT entries are not aligned")
	}
	pat := &PAT{
		TransportStreamID: uint16(data[3])<<8 | uint16(data[4]),
		Version:           (data[5] >> 1) & 0x1f,
		CurrentNext:       data[5]&1 != 0,
		SectionNumber:     data[6],
		LastSection:       data[7],
	}
	for i := 8; i < bodyEnd; i += 4 {
		program := uint16(data[i])<<8 | uint16(data[i+1])
		if program == 0 {
			continue
		}
		pat.Programs = append(pat.Programs, PATProgram{ProgramNumber: program, PMTPID: uint16(data[i+2]&0x1f)<<8 | uint16(data[i+3])})
	}
	sort.Slice(pat.Programs, func(i, j int) bool { return pat.Programs[i].ProgramNumber < pat.Programs[j].ProgramNumber })
	return pat, nil
}

func ParsePMT(data []byte) (*PMT, error) {
	if len(data) < 16 || data[0] != tableIDPMT {
		return nil, fmt.Errorf("psi: malformed PMT")
	}
	if err := checkSectionLength(data); err != nil {
		return nil, err
	}
	bodyEnd := len(data) - 4
	if 12 > bodyEnd {
		return nil, fmt.Errorf("psi: PMT header is truncated")
	}
	programInfoLen := int(data[10]&0x0f)<<8 | int(data[11])
	cursor := 12
	if cursor+programInfoLen > bodyEnd {
		return nil, fmt.Errorf("psi: PMT program_info_length exceeds section")
	}
	cursor += programInfoLen
	pmt := &PMT{
		ProgramNumber:  uint16(data[3])<<8 | uint16(data[4]),
		Version:        (data[5] >> 1) & 0x1f,
		CurrentNext:    data[5]&1 != 0,
		SectionNumber:  data[6],
		LastSection:    data[7],
		PCRPID:         uint16(data[8]&0x1f)<<8 | uint16(data[9]),
		ProgramInfoLen: uint16(programInfoLen),
	}
	for cursor < bodyEnd {
		if cursor+5 > bodyEnd {
			return nil, fmt.Errorf("psi: PMT elementary stream header is truncated")
		}
		stream := Stream{
			StreamType: data[cursor],
			PID:        uint16(data[cursor+1]&0x1f)<<8 | uint16(data[cursor+2]),
		}
		infoLen := int(data[cursor+3]&0x0f)<<8 | int(data[cursor+4])
		cursor += 5
		if cursor+infoLen > bodyEnd {
			return nil, fmt.Errorf("psi: PMT ES_info_length exceeds section")
		}
		cursor += infoLen
		pmt.Streams = append(pmt.Streams, stream)
	}
	return pmt, nil
}

func checkSectionLength(data []byte) error {
	declared := (int(data[1]&0x0f) << 8) | int(data[2])
	if declared+3 != len(data) {
		return fmt.Errorf("psi: section_length %d does not match %d bytes", declared, len(data))
	}
	return nil
}
