// Package psi reassembles and parses MPEG program-specific information tables.
package psi

import (
	"crypto/sha256"
	"errors"
	"fmt"

	"tsaudit/internal/tspipe"
)

const (
	TablePAT = 0x00
	TablePMT = 0x02
)

// Section is a complete, CRC-checked PSI section.
type Section struct {
	TableID           uint8
	TableIDExtension  uint16
	Version           uint8
	CurrentNext       bool
	SectionNumber     uint8
	LastSectionNumber uint8
	Body              []byte
	Raw               []byte
	Hash              [32]byte
	StartOffset       int64
	EndOffset         int64
}

type sectionState struct {
	buf   []byte
	start int64
}

// Assembler handles pointer fields, sections spanning packets, multiple
// sections per packet, duplicate retransmissions, and CRC validation.
type Assembler struct {
	states            map[uint16]*sectionState
	seen              map[uint16]map[[32]byte]struct{}
	CRCErrors         int
	Malformed         int
	DuplicateSections int
}

func NewAssembler() *Assembler {
	return &Assembler{
		states: make(map[uint16]*sectionState),
		seen:   make(map[uint16]map[[32]byte]struct{}),
	}
}

func (a *Assembler) Feed(p *tspipe.Packet) []Section {
	if p == nil || !p.HasPayload || len(p.Payload) == 0 {
		return nil
	}
	if p.PUSI {
		return a.feedPUSI(p)
	}
	st := a.states[p.PID]
	if st == nil || len(st.buf) == 0 {
		return nil
	}
	return a.consume(p.PID, p.Payload, p.Offset)
}

func (a *Assembler) feedPUSI(p *tspipe.Packet) []Section {
	pointer := int(p.Payload[0])
	if pointer > len(p.Payload)-1 {
		a.Malformed++
		a.reset(p.PID)
		return nil
	}
	var out []Section
	if pointer > 0 {
		if st := a.states[p.PID]; st != nil && len(st.buf) > 0 {
			out = a.consume(p.PID, p.Payload[1:1+pointer], p.Offset)
		}
	}
	if st := a.states[p.PID]; st != nil && len(st.buf) > 0 {
		// The pointer bytes must finish the prior section.
		a.Malformed++
		a.reset(p.PID)
	}
	if 1+pointer < len(p.Payload) {
		out = append(out, a.consume(p.PID, p.Payload[1+pointer:], p.Offset)...)
	}
	return out
}

func (a *Assembler) consume(pid uint16, data []byte, offset int64) []Section {
	st := a.states[pid]
	if st == nil {
		st = &sectionState{}
		a.states[pid] = st
	}
	if len(st.buf) == 0 {
		st.start = offset
	}
	st.buf = append(st.buf, data...)

	var out []Section
	for {
		if len(st.buf) < 3 {
			return out
		}
		if st.buf[0] == 0xff {
			a.reset(pid)
			return out
		}
		sectionLength := int(st.buf[1]&0x0f)<<8 | int(st.buf[2])
		total := 3 + sectionLength
		if sectionLength < 9 || sectionLength > 4093 {
			a.Malformed++
			a.reset(pid)
			return out
		}
		if len(st.buf) < total {
			return out
		}

		raw := append([]byte(nil), st.buf[:total]...)
		st.buf = append(st.buf[:0], st.buf[total:]...)
		if len(st.buf) == 0 {
			st.start = offset
		}
		sec, err := parseSection(raw, st.start, offset)
		if err != nil {
			a.Malformed++
			continue
		}
		if !a.validCRC(sec.Raw) {
			a.CRCErrors++
			continue
		}
		if a.isDuplicate(pid, sec.Hash) {
			a.DuplicateSections++
			continue
		}
		out = append(out, *sec)
	}
}

func (a *Assembler) validCRC(raw []byte) bool {
	return CRC32MPEG(raw) == 0
}

func (a *Assembler) isDuplicate(pid uint16, hash [32]byte) bool {
	m := a.seen[pid]
	if m == nil {
		m = make(map[[32]byte]struct{})
		a.seen[pid] = m
	}
	if _, ok := m[hash]; ok {
		return true
	}
	m[hash] = struct{}{}
	return false
}

func (a *Assembler) reset(pid uint16) {
	if st := a.states[pid]; st != nil {
		st.buf = nil
		st.start = 0
	}
}

func parseSection(raw []byte, start, end int64) (*Section, error) {
	if len(raw) < 12 {
		return nil, errors.New("psi: section too short")
	}
	sectionLength := int(raw[1]&0x0f)<<8 | int(raw[2])
	if 3+sectionLength != len(raw) {
		return nil, fmt.Errorf("psi: section length mismatch: %d != %d", sectionLength, len(raw)-3)
	}
	last := raw[7]
	number := raw[6]
	if last < number {
		return nil, errors.New("psi: invalid section numbering")
	}
	return &Section{
		TableID:           raw[0],
		TableIDExtension:  uint16(raw[3])<<8 | uint16(raw[4]),
		Version:           (raw[5] >> 1) & 0x1f,
		CurrentNext:       raw[5]&1 != 0,
		SectionNumber:     number,
		LastSectionNumber: last,
		Body:              append([]byte(nil), raw[8:len(raw)-4]...),
		Raw:               raw,
		Hash:              sha256.Sum256(raw),
		StartOffset:       start,
		EndOffset:         end,
	}, nil
}

// CRC32MPEG computes the MPEG-2 CRC-32. A section with its CRC appended
// correctly evaluates to zero.
func CRC32MPEG(data []byte) uint32 {
	crc := uint32(0xffffffff)
	for _, b := range data {
		crc ^= uint32(b) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
