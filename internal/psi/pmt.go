package psi

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

type Descriptor struct {
	Tag  uint8  `json:"tag"`
	Data []byte `json:"data,omitempty"`
}

type ESInfo struct {
	PID         uint16       `json:"pid"`
	StreamType  uint8        `json:"stream_type"`
	Descriptors []Descriptor `json:"descriptors,omitempty"`
}

// PMT is a complete, merged Program Map Table for one program.
type PMT struct {
	ProgramNumber uint16
	Version       uint8
	PCRPID        uint16
	ProgramInfo   []Descriptor
	Streams       []ESInfo
	ContentHash   [32]byte
}

type PMTSection struct {
	ProgramNumber     uint16
	Version           uint8
	CurrentNext       bool
	SectionNumber     uint8
	LastSectionNumber uint8
	PCRPID            uint16
	ProgramInfo       []Descriptor
	Streams           []ESInfo
}

func ParsePMT(sec Section) (*PMTSection, error) {
	if sec.TableID != TablePMT {
		return nil, fmt.Errorf("psi: expected PMT, got table 0x%02x", sec.TableID)
	}
	if len(sec.Body) < 4 {
		return nil, errors.New("psi: PMT body is too short")
	}
	pcrPID := binary.BigEndian.Uint16(sec.Body[0:2]) & 0x1fff
	infoLength := int(binary.BigEndian.Uint16(sec.Body[2:4]) & 0x0fff)
	if 4+infoLength > len(sec.Body) {
		return nil, errors.New("psi: program descriptor length exceeds PMT body")
	}
	info, err := parseDescriptors(sec.Body[4 : 4+infoLength])
	if err != nil {
		return nil, err
	}
	rest := sec.Body[4+infoLength:]
	streams := make([]ESInfo, 0, 4)
	for len(rest) > 0 {
		if len(rest) < 5 {
			return nil, errors.New("psi: truncated elementary stream entry")
		}
		streamType := rest[0]
		pid := binary.BigEndian.Uint16(rest[1:3]) & 0x1fff
		esInfoLength := int(binary.BigEndian.Uint16(rest[3:5]) & 0x0fff)
		if 5+esInfoLength > len(rest) {
			return nil, errors.New("psi: elementary stream descriptor length exceeds PMT body")
		}
		descs, err := parseDescriptors(rest[5 : 5+esInfoLength])
		if err != nil {
			return nil, err
		}
		streams = append(streams, ESInfo{PID: pid, StreamType: streamType, Descriptors: descs})
		rest = rest[5+esInfoLength:]
	}
	return &PMTSection{
		ProgramNumber:     sec.TableIDExtension,
		Version:           sec.Version,
		CurrentNext:       sec.CurrentNext,
		SectionNumber:     sec.SectionNumber,
		LastSectionNumber: sec.LastSectionNumber,
		PCRPID:            pcrPID,
		ProgramInfo:       info,
		Streams:           streams,
	}, nil
}

func parseDescriptors(data []byte) ([]Descriptor, error) {
	var out []Descriptor
	for len(data) > 0 {
		if len(data) < 2 {
			return nil, errors.New("psi: truncated descriptor header")
		}
		length := int(data[1])
		if 2+length > len(data) {
			return nil, errors.New("psi: descriptor length exceeds its container")
		}
		out = append(out, Descriptor{Tag: data[0], Data: append([]byte(nil), data[2:2+length]...)})
		data = data[2+length:]
	}
	return out, nil
}

// PMTMerger merges sections belonging to one PMT version.
type PMTMerger struct {
	program  uint16
	version  uint8
	last     uint8
	sections map[uint8]*PMTSection
}

func (m *PMTMerger) Add(sec Section) (*PMT, bool, error) {
	part, err := ParsePMT(sec)
	if err != nil {
		return nil, false, err
	}
	if m.sections == nil || m.version != part.Version || m.program != part.ProgramNumber {
		m.program = part.ProgramNumber
		m.version = part.Version
		m.last = part.LastSectionNumber
		m.sections = make(map[uint8]*PMTSection)
	}
	if part.LastSectionNumber != m.last || int(part.SectionNumber) > int(m.last) {
		return nil, false, errors.New("psi: inconsistent PMT section numbering")
	}
	m.sections[part.SectionNumber] = part
	if len(m.sections) != int(m.last)+1 {
		return nil, false, nil
	}
	first := m.sections[0]
	if first == nil {
		return nil, false, nil
	}
	out := &PMT{
		ProgramNumber: m.program,
		Version:       m.version,
		PCRPID:        first.PCRPID,
		ProgramInfo:   first.ProgramInfo,
	}
	byPID := make(map[uint16]ESInfo)
	for i := 0; i <= int(m.last); i++ {
		part := m.sections[uint8(i)]
		if part == nil {
			return nil, false, nil
		}
		for _, es := range part.Streams {
			byPID[es.PID] = es
		}
	}
	for _, es := range byPID {
		out.Streams = append(out.Streams, es)
	}
	sort.Slice(out.Streams, func(i, j int) bool { return out.Streams[i].PID < out.Streams[j].PID })
	out.ContentHash = pmtHash(out)
	return out, true, nil
}

func pmtHash(p *PMT) [32]byte {
	h := sha256.New()
	var b [4]byte
	binary.BigEndian.PutUint16(b[0:2], p.PCRPID)
	h.Write(b[0:2])
	writeDescriptors(h, p.ProgramInfo)
	binary.BigEndian.PutUint16(b[0:2], uint16(len(p.Streams)))
	h.Write(b[0:2])
	streams := append([]ESInfo(nil), p.Streams...)
	sort.Slice(streams, func(i, j int) bool { return streams[i].PID < streams[j].PID })
	for _, es := range streams {
		b[0] = es.StreamType
		binary.BigEndian.PutUint16(b[1:3], es.PID)
		h.Write(b[:3])
		writeDescriptors(h, es.Descriptors)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func writeDescriptors(h interface{ Write([]byte) (int, error) }, descs []Descriptor) {
	var b [2]byte
	for _, d := range descs {
		b[0], b[1] = d.Tag, uint8(len(d.Data))
		h.Write(b[:])
		h.Write(d.Data)
	}
}
