package psi

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math/bits"
)

const (
	MaxSectionLength = 4093
	MinSectionLength = 9
)

// Section is the result of assembling one PSI section. Invalid sections are
// returned as diagnostics and must not be applied to state.
type Section struct {
	PID      uint16
	Data     []byte
	CRCValid bool
	Err      string
}

type sectionState struct {
	buf    []byte
	active bool
}

// Assembler handles PSI sections per PID. It supports non-zero pointer fields,
// sections spanning packets and multiple sections in one payload.
type Assembler struct {
	states        map[uint16]*sectionState
	CRCErrors     int
	LengthErrors  int
	PointerErrors int
}

func NewAssembler() *Assembler {
	return &Assembler{states: make(map[uint16]*sectionState)}
}

func (a *Assembler) state(pid uint16) *sectionState {
	st := a.states[pid]
	if st == nil {
		st = &sectionState{}
		a.states[pid] = st
	}
	return st
}

// Feed consumes one packet payload for pid and returns all completed sections.
func (a *Assembler) Feed(pid uint16, pusi bool, payload []byte) []Section {
	if len(payload) == 0 {
		return nil
	}
	st := a.state(pid)
	var out []Section
	if pusi {
		if payload[0] > byte(len(payload)-1) {
			a.PointerErrors++
			st.buf = st.buf[:0]
			st.active = false
			return []Section{{PID: pid, Err: "pointer_field exceeds payload"}}
		}
		pointer := int(payload[0])
		if pointer > 0 {
			a.consumePointerTail(pid, st, payload[1:1+pointer], &out)
		}
		st.buf = st.buf[:0]
		st.active = false
		a.consume(pid, st, payload[1+pointer:], &out)
		return out
	}
	if !st.active {
		return nil
	}
	a.consume(pid, st, payload, &out)
	return out
}

func (a *Assembler) consumePointerTail(pid uint16, st *sectionState, tail []byte, out *[]Section) {
	if !st.active || len(tail) == 0 {
		return
	}
	st.buf = append(st.buf, tail...)
	if len(st.buf) < 3 {
		a.LengthErrors++
		st.buf = st.buf[:0]
		st.active = false
		return
	}
	total := 3 + sectionLength(st.buf[1:3])
	if total < 3+MinSectionLength || total > 3+MaxSectionLength || len(st.buf) < total {
		a.LengthErrors++
		st.buf = st.buf[:0]
		st.active = false
		return
	}
	*out = append(*out, a.finish(pid, st.buf[:total]))
	st.buf = st.buf[:0]
	st.active = false
}

func (a *Assembler) consume(pid uint16, st *sectionState, data []byte, out *[]Section) {
	for len(data) > 0 || st.active {
		if !st.active {
			if len(data) == 0 || data[0] == 0xff {
				return
			}
			st.buf = st.buf[:0]
			st.active = true
		}
		if len(st.buf) < 3 {
			n := 3 - len(st.buf)
			if n > len(data) {
				n = len(data)
			}
			st.buf = append(st.buf, data[:n]...)
			data = data[n:]
			if len(st.buf) < 3 {
				return
			}
		}
		length := sectionLength(st.buf[1:3])
		if length < MinSectionLength || length > MaxSectionLength {
			a.LengthErrors++
			*out = append(*out, Section{PID: pid, Err: fmt.Sprintf("invalid section_length %d", length)})
			st.buf = st.buf[:0]
			st.active = false
			return
		}
		total := 3 + length
		n := total - len(st.buf)
		if n > len(data) {
			n = len(data)
		}
		st.buf = append(st.buf, data[:n]...)
		data = data[n:]
		if len(st.buf) < total {
			return
		}
		*out = append(*out, a.finish(pid, st.buf[:total]))
		st.buf = st.buf[:0]
		st.active = false
	}
}

func (a *Assembler) finish(pid uint16, data []byte) Section {
	sec := Section{PID: pid, Data: append([]byte(nil), data...)}
	sec.CRCValid = CRC32(sec.Data) == 0
	if !sec.CRCValid {
		a.CRCErrors++
	}
	return sec
}

func sectionLength(b []byte) int {
	return int(b[0]&0x0f)<<8 | int(b[1])
}

var mpeg2ReflectedTable = crc32.MakeTable(0xedb88320)

// CRC32 implements the MPEG-2 PSI CRC (poly 0x04c11db7, init 0xffffffff,
// no final xor). A section including its trailing CRC hashes to zero.
func CRC32(data []byte) uint32 {
	reversed := make([]byte, len(data))
	for i, b := range data {
		reversed[i] = bits.Reverse8(b)
	}
	reflected := ^crc32.Update(0, mpeg2ReflectedTable, reversed)
	return bits.Reverse32(reflected)
}

// AppendCRC appends the four big-endian CRC bytes for data.
func AppendCRC(data []byte) []byte {
	crc := CRC32(data)
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], crc)
	return append(data, b[:]...)
}
