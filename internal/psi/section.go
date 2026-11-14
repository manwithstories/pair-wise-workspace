// Package psi implements MPEG-TS PSI section reassembly and table parsing.
package psi

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	TableIDPAT = 0x00
	TableIDPMT = 0x02
)

var (
	ErrSectionCRC  = errors.New("psi: section CRC mismatch")
	ErrSectionSize = errors.New("psi: invalid section size")
)

// Section is a validated PSI section. Body excludes the generic section header
// and the trailing CRC.
type Section struct {
	PID               uint16
	TableID           uint8
	TableIDExtension  uint16
	Version           uint8
	CurrentNext       bool
	SectionNumber     uint8
	LastSectionNumber uint8
	Body              []byte
	Raw               []byte
	StartPacket       int64
}

// ReassemblyStats reports section-level damage without stopping the scan.
type ReassemblyStats struct {
	Sections  int64
	CRCErrors int64
	Discarded int64
	Resyncs   int64
}

type sectionState struct {
	buf   []byte
	need  int
	start int64
	has   bool
}

// Reassembler reconstructs sections for each PSI PID. It tolerates pointer
// fields, multiple sections per packet, sections spanning packets and retransmission.
type Reassembler struct {
	states  map[uint16]*sectionState
	handler func(*Section)
	stats   ReassemblyStats
}

// NewReassembler creates a reassembler that emits CRC-valid sections.
func NewReassembler(handler func(*Section)) *Reassembler {
	return &Reassembler{states: make(map[uint16]*sectionState), handler: handler}
}

// Stats returns reassembly counters.
func (r *Reassembler) Stats() ReassemblyStats { return r.stats }

// Feed consumes the payload of one TS packet for a PSI PID.
func (r *Reassembler) Feed(pid uint16, pusi bool, payload []byte, packet int64) {
	if len(payload) == 0 {
		return
	}
	st := r.states[pid]
	if st == nil {
		st = &sectionState{}
		r.states[pid] = st
	}
	if pusi {
		pointer := int(payload[0])
		if pointer+1 > len(payload) {
			r.stats.Discarded++
			r.reset(st)
			return
		}
		if st.has && pointer > 0 {
			r.consume(pid, st, payload[1:1+pointer])
		}
		if st.has {
			// A new section starts here, so an unfinished prior section is damage.
			r.stats.Discarded++
			r.reset(st)
		}
		st.start = packet
		r.consume(pid, st, payload[1+pointer:])
		return
	}
	if !st.has {
		return
	}
	r.consume(pid, st, payload)
}

func (r *Reassembler) reset(st *sectionState) {
	st.buf = st.buf[:0]
	st.need = 0
	st.has = false
}

func (r *Reassembler) consume(pid uint16, st *sectionState, data []byte) {
	for len(data) > 0 {
		if !st.has {
			if data[0] == 0xff {
				return
			}
			st.buf = append(st.buf[:0], data...)
			st.has = true
		} else {
			st.buf = append(st.buf, data...)
		}
		data = nil

		for len(st.buf) > 0 {
			if st.buf[0] == 0xff {
				r.reset(st)
				return
			}
			if len(st.buf) < 3 {
				return
			}
			length := int(st.buf[1]&0x0f)<<8 | int(st.buf[2])
			if length < 4 || length > 1021 {
				r.stats.Discarded++
				st.buf = st.buf[1:]
				st.need = 0
				continue
			}
			total := 3 + length
			if len(st.buf) < total {
				st.need = total
				return
			}
			raw := st.buf[:total]
			if err := verifySectionCRC(raw); err != nil {
				r.stats.CRCErrors++
				r.stats.Discarded++
				st.buf = st.buf[total:]
				st.need = 0
				continue
			}
			sec, err := parseSection(pid, raw, st.start)
			if err != nil {
				r.stats.Discarded++
			} else {
				r.stats.Sections++
				if r.handler != nil {
					r.handler(sec)
				}
			}
			st.buf = st.buf[total:]
			st.need = 0
		}
		r.reset(st)
	}
}

func parseSection(pid uint16, raw []byte, start int64) (*Section, error) {
	if len(raw) < 8 {
		return nil, ErrSectionSize
	}
	length := int(raw[1]&0x0f)<<8 | int(raw[2])
	total := 3 + length
	if total != len(raw) || length < 4 {
		return nil, ErrSectionSize
	}
	s := &Section{
		PID:         pid,
		TableID:     raw[0],
		StartPacket: start,
		Raw:         append([]byte(nil), raw...),
	}
	bodyStart := 3
	if raw[1]&0x80 != 0 {
		if len(raw) < 12 {
			return nil, ErrSectionSize
		}
		s.TableIDExtension = binary.BigEndian.Uint16(raw[3:5])
		s.Version = (raw[5] >> 1) & 0x1f
		s.CurrentNext = raw[5]&1 != 0
		s.SectionNumber = raw[6]
		s.LastSectionNumber = raw[7]
		bodyStart = 8
	}
	s.Body = append([]byte(nil), raw[bodyStart:total-4]...)
	return s, nil
}

func verifySectionCRC(raw []byte) error {
	if len(raw) < 4 || CRC32MPEG2(raw[:len(raw)-4]) != binary.BigEndian.Uint32(raw[len(raw)-4:]) {
		return ErrSectionCRC
	}
	return nil
}

// CRC32MPEG2 computes the MPEG-2 PSI CRC (polynomial 0x04C11DB7, init all ones,
// no final XOR). Verification compares this value over the section excluding
// the trailing field with the field itself.
func CRC32MPEG2(data []byte) uint32 {
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

// AppendCRC returns section bytes with the CRC appended.
func AppendCRC(section []byte) []byte {
	out := make([]byte, len(section)+4)
	copy(out, section)
	binary.BigEndian.PutUint32(out[len(section):], CRC32MPEG2(section))
	return out
}

// SetSectionLength fills the 12-bit section_length field.
func SetSectionLength(section []byte) error {
	if len(section) < 3 {
		return fmt.Errorf("psi: section too short")
	}
	n := len(section) - 3
	if n > 1021 {
		return fmt.Errorf("psi: section too long: %d", n)
	}
	section[1] = section[1]&0xf0 | byte(n>>8)&0x0f
	section[2] = byte(n)
	return nil
}

// FinalizeSection sets section_length to include the trailing CRC and appends
// the CRC over the correctly finalized header and body.
func FinalizeSection(section []byte) []byte {
	n := len(section) - 3 + 4
	section[1] = section[1]&0xf0 | byte(n>>8)&0x0f
	section[2] = byte(n)
	return AppendCRC(section)
}
