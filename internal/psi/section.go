// Package psi reassembles and parses MPEG-TS program specific information.
package psi

import (
	"encoding/binary"
	"fmt"
	"sort"
)

const (
	TableIDPAT = 0x00
	TableIDPMT = 0x02
	TableIDSDT = 0x42

	maxSectionSize = 3 + 0x0fff
)

// Section is one CRC-valid PSI/SI section.
type Section struct {
	TableID           uint8
	PID               uint16
	Version           uint8
	CurrentNext       bool
	SectionNumber     uint8
	LastSectionNumber uint8
	Body              []byte
	Raw               []byte
	PacketIndex       uint64
	ByteOffset        int64
}

// SectionError describes a section that was discarded.
type SectionError struct {
	PID         uint16
	TableID     uint8
	PacketIndex uint64
	ByteOffset  int64
	Reason      string
	CRC         bool
}

type pending struct {
	raw         []byte
	total       int
	packetIndex uint64
	byteOffset  int64
}

// Assembler performs pointer_field handling and cross-packet section assembly.
type Assembler struct {
	pending    map[uint16]*pending
	crcErrors  uint64
	incomplete uint64
}

// NewAssembler creates an empty section assembler.
func NewAssembler() *Assembler {
	return &Assembler{pending: make(map[uint16]*pending)}
}

// Push consumes one PSI packet payload and returns complete, CRC-valid sections.
func (a *Assembler) Push(pid uint16, payload []byte, pusi bool, packetIndex uint64, byteOffset int64) ([]Section, []SectionError) {
	if !pusi {
		if p := a.pending[pid]; p != nil {
			return a.feed(pid, p, payload, packetIndex, byteOffset)
		}
		return nil, nil
	}
	if len(payload) == 0 {
		a.discard(pid)
		return nil, nil
	}
	pointer := int(payload[0])
	if pointer > len(payload)-1 {
		a.discard(pid)
		a.incomplete++
		return nil, []SectionError{{PID: pid, PacketIndex: packetIndex, ByteOffset: byteOffset, Reason: "pointer_field exceeds payload"}}
	}

	var sections []Section
	var errs []SectionError
	if p := a.pending[pid]; p != nil {
		s, e := a.feed(pid, p, payload[1:1+pointer], packetIndex, byteOffset)
		sections = append(sections, s...)
		errs = append(errs, e...)
		if a.pending[pid] != nil {
			a.discard(pid)
		}
	}
	s, e := a.feed(pid, nil, payload[1+pointer:], packetIndex, byteOffset)
	return append(sections, s...), append(errs, e...)
}

func (a *Assembler) feed(pid uint16, p *pending, data []byte, packetIndex uint64, byteOffset int64) ([]Section, []SectionError) {
	var sections []Section
	var errs []SectionError
	for len(data) > 0 || (p != nil && p.total > 0 && len(p.raw) >= p.total) {
		if p == nil {
			if data[0] == 0xff {
				break
			}
			p = &pending{packetIndex: packetIndex, byteOffset: byteOffset}
			a.pending[pid] = p
		}
		if len(p.raw) < 3 {
			n := 3 - len(p.raw)
			if n > len(data) {
				n = len(data)
			}
			p.raw = append(p.raw, data[:n]...)
			data = data[n:]
			if len(p.raw) < 3 {
				break
			}
			p.total = 3 + int(binary.BigEndian.Uint16(p.raw[1:3])&0x0fff)
			if p.total < 12 || p.total > maxSectionSize {
				a.discard(pid)
				a.incomplete++
				errs = append(errs, SectionError{PID: pid, TableID: p.raw[0], Reason: "invalid section_length"})
				p = nil
				continue
			}
		}
		need := p.total - len(p.raw)
		if need > 0 {
			if need > len(data) {
				need = len(data)
			}
			p.raw = append(p.raw, data[:need]...)
			data = data[need:]
		}
		if len(p.raw) < p.total {
			break
		}
		raw := append([]byte(nil), p.raw...)
		startIndex, startOffset := p.packetIndex, p.byteOffset
		delete(a.pending, pid)
		p = nil
		sec, err := parseCompleteSection(pid, raw, startIndex, startOffset)
		if err != nil {
			a.crcErrors++
			errs = append(errs, SectionError{PID: pid, TableID: raw[0], PacketIndex: startIndex, ByteOffset: startOffset, Reason: err.Error(), CRC: true})
			continue
		}
		sections = append(sections, sec)
	}
	return sections, errs
}

func parseCompleteSection(pid uint16, raw []byte, idx uint64, off int64) (Section, error) {
	if len(raw) < 12 {
		return Section{}, fmt.Errorf("section too short: %d", len(raw))
	}
	want := CRC32MPEG(raw[:len(raw)-4])
	got := binary.BigEndian.Uint32(raw[len(raw)-4:])
	if want != got {
		return Section{}, fmt.Errorf("CRC mismatch: got %08x want %08x", got, want)
	}
	syntax := raw[1]&0x80 != 0
	if !syntax {
		return Section{}, fmt.Errorf("unsupported section syntax")
	}
	return Section{
		TableID:           raw[0],
		PID:               pid,
		Version:           (raw[5] >> 1) & 0x1f,
		CurrentNext:       raw[5]&0x01 != 0,
		SectionNumber:     raw[6],
		LastSectionNumber: raw[7],
		Body:              append([]byte(nil), raw[8:len(raw)-4]...),
		Raw:               raw,
		PacketIndex:       idx,
		ByteOffset:        off,
	}, nil
}

func (a *Assembler) discard(pid uint16) {
	if a.pending[pid] != nil {
		a.pending[pid] = nil
		a.incomplete++
	}
}

// CRCErrors returns the number of completed sections rejected for CRC mismatch.
func (a *Assembler) CRCErrors() uint64 { return a.crcErrors }

// Incomplete returns the number of abandoned partial sections.
func (a *Assembler) Incomplete() uint64 { return a.incomplete }

// Flush reports and discards sections left incomplete at end of input.
func (a *Assembler) Flush() []SectionError {
	if len(a.pending) == 0 {
		return nil
	}
	var pids []uint16
	for pid := range a.pending {
		pids = append(pids, pid)
	}
	sort.Slice(pids, func(i, j int) bool { return pids[i] < pids[j] })
	var errs []SectionError
	for _, pid := range pids {
		p := a.pending[pid]
		if p == nil {
			delete(a.pending, pid)
			continue
		}
		tableID := uint8(0)
		if len(p.raw) > 0 {
			tableID = p.raw[0]
		}
		errs = append(errs, SectionError{PID: pid, TableID: tableID, PacketIndex: p.packetIndex, ByteOffset: p.byteOffset, Reason: "incomplete section at EOF"})
		delete(a.pending, pid)
		a.incomplete++
	}
	return errs
}

var crcTable = makeCRCTable()

func makeCRCTable() [256]uint32 {
	var t [256]uint32
	for i := 0; i < 256; i++ {
		c := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if c&0x80000000 != 0 {
				c = (c << 1) ^ 0x04c11db7
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}

// CRC32MPEG computes the MPEG-2 PSI CRC (no final XOR).
func CRC32MPEG(bs []byte) uint32 {
	crc := uint32(0xffffffff)
	for _, b := range bs {
		crc = (crc << 8) ^ crcTable[byte(crc>>24)^b]
	}
	return crc
}
