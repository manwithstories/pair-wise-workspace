package transport

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// PSI errors are countable without tearing down the last valid table.
var (
	ErrBadCRC            = errors.New("PSI CRC mismatch")
	ErrMalformedPSI      = errors.New("malformed PSI section")
	ErrVersionConflict   = errors.New("PSI version conflict")
	ErrPSIWaitingConfirm = errors.New("PSI new version awaits consistency confirmation")
)

// CRC32MPEG computes CRC-32/MPEG-2: init=0xffffffff, MSB-first polynomial
// 0x04c11db7, no reflection and no final xor.
func CRC32MPEG(data []byte) uint32 {
	const polynomial = uint32(0x04c11db7)
	crc := uint32(0xffffffff)
	for _, b := range data {
		crc ^= uint32(b) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ polynomial
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// PSITable is a validated PAT/PMT section.
type PSITable struct {
	PID           uint16
	TableID       byte
	Extension     uint16
	Version       byte
	CurrentNext   bool
	SectionNumber byte
	LastSection   byte
	Payload       []byte
	CRC           uint32
	ContentHash   uint32
}

// BuildSection builds a complete PSI section, including CRC but excluding the
// TS pointer_field.
func BuildSection(tableID byte, extension uint16, version byte, payload []byte) []byte {
	sectionLen := 5 + len(payload) + 4 // ext/version/section/last + payload + CRC
	s := make([]byte, 3+sectionLen)
	s[0] = tableID
	s[1] = 0xb0 | byte((sectionLen>>8)&0x0f)
	s[2] = byte(sectionLen)
	binary.BigEndian.PutUint16(s[3:5], extension)
	s[5] = 0xc0 | ((version & 0x1f) << 1) | 0x01 // current_next=1
	s[6] = 0
	s[7] = 0
	copy(s[8:], payload)
	crc := CRC32MPEG(s[:len(s)-4])
	binary.BigEndian.PutUint32(s[len(s)-4:], crc)
	return s
}

// BuildPSIPayload adds the one-byte pointer field used in PUSI TS packets.
func BuildPSIPayload(tableID byte, extension uint16, version byte, entries []byte) []byte {
	section := BuildSection(tableID, extension, version, entries)
	out := make([]byte, 1+len(section))
	out[0] = 0x00
	copy(out[1:], section)
	return out
}

// ParsePSIPayload parses a PUSI payload beginning with pointer_field.
func ParsePSIPayload(pid uint16, payload []byte) (*PSITable, error) {
	if len(payload) < 2 {
		return nil, fmt.Errorf("%w: short pointer payload", ErrMalformedPSI)
	}
	pointer := int(payload[0])
	start := 1 + pointer
	if start >= len(payload) {
		return nil, fmt.Errorf("%w: pointer %d out of range", ErrMalformedPSI, pointer)
	}
	return ParseSection(pid, payload[start:])
}

// ParseSection validates length and CRC and returns a table. On CRC failure
// the returned table is nil; callers must retain their previous valid table.
func ParseSection(pid uint16, s []byte) (*PSITable, error) {
	if len(s) < 8 {
		return nil, fmt.Errorf("%w: section too short", ErrMalformedPSI)
	}
	sectionLen := int(uint16(s[1]&0x0f)<<8 | uint16(s[2]))
	total := sectionLen + 3
	if sectionLen < 9 || total > len(s) {
		return nil, fmt.Errorf("%w: section_length=%d available=%d", ErrMalformedPSI, sectionLen, len(s))
	}
	body := s[:total]
	wantCRC := binary.BigEndian.Uint32(body[total-4:])
	gotCRC := CRC32MPEG(body[:total-4])
	if wantCRC != gotCRC {
		return nil, fmt.Errorf("%w: pid=0x%x want=%08x got=%08x", ErrBadCRC, pid, wantCRC, gotCRC)
	}
	t := &PSITable{
		PID:           pid,
		TableID:       body[0],
		Extension:     binary.BigEndian.Uint16(body[3:5]),
		Version:       (body[5] >> 1) & 0x1f,
		CurrentNext:   body[5]&0x01 == 1,
		SectionNumber: body[6],
		LastSection:   body[7],
		Payload:       append([]byte(nil), body[8:total-4]...),
		CRC:           wantCRC,
		ContentHash:   CRC32MPEG(body[8 : total-4]),
	}
	if t.SectionNumber != 0 || t.LastSection != 0 {
		return nil, fmt.Errorf("%w: multi-section PSI is not used by fixtures", ErrMalformedPSI)
	}
	if !t.CurrentNext {
		return nil, fmt.Errorf("%w: next table not yet current", ErrVersionConflict)
	}
	return t, nil
}

// PATProgram is one PAT program entry.
type PATProgram struct {
	ProgramNumber uint16
	PMTPID        uint16
}

func BuildPATEntries(programNumber, pmtPID uint16) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b[0:2], programNumber)
	b[2] = 0xe0 | byte((pmtPID>>8)&0x1f)
	b[3] = byte(pmtPID)
	return b
}

func ParsePATEntries(t *PSITable) ([]PATProgram, error) {
	if len(t.Payload)%4 != 0 {
		return nil, fmt.Errorf("%w: PAT entries length %d", ErrMalformedPSI, len(t.Payload))
	}
	var out []PATProgram
	for i := 0; i < len(t.Payload); i += 4 {
		pn := binary.BigEndian.Uint16(t.Payload[i : i+2])
		pid := uint16(t.Payload[i+2]&0x1f)<<8 | uint16(t.Payload[i+3])
		if pn != 0 { // network PID entry has program_number 0
			out = append(out, PATProgram{ProgramNumber: pn, PMTPID: pid})
		}
	}
	return out, nil
}

// PMTStream is an elementary stream in the generated PMT.
type PMTStream struct {
	StreamType byte
	PID        uint16
	Codec      string
}

const (
	StreamTypeH264     = 0x1b
	StreamTypePCMS16LE = 0x83 // private/descriptor-tagged signed PCM in fixtures
)

func BuildPMTEntries(pcrPID uint16, streams []PMTStream) []byte {
	head := make([]byte, 4)
	head[0] = 0xe0 | byte((pcrPID>>8)&0x1f)
	head[1] = byte(pcrPID)
	head[2] = 0xf0
	head[3] = 0x00 // program info length zero
	for _, st := range streams {
		e := make([]byte, 5)
		e[0] = st.StreamType
		e[1] = 0xe0 | byte((st.PID>>8)&0x1f)
		e[2] = byte(st.PID)
		e[3] = 0xf0
		e[4] = 0x00 // elementary stream info length zero
		head = append(head, e...)
	}
	return head
}

func ParsePMTEntries(t *PSITable) (pcrPID uint16, streams []PMTStream, err error) {
	if len(t.Payload) < 4 {
		return 0, nil, fmt.Errorf("%w: short PMT", ErrMalformedPSI)
	}
	pcrPID = uint16(t.Payload[0]&0x1f)<<8 | uint16(t.Payload[1])
	infoLen := int(uint16(t.Payload[2]&0x0f)<<8 | uint16(t.Payload[3]))
	pos := 4 + infoLen
	for pos+5 <= len(t.Payload) {
		st := t.Payload[pos]
		pid := uint16(t.Payload[pos+1]&0x1f)<<8 | uint16(t.Payload[pos+2])
		esLen := int(uint16(t.Payload[pos+3]&0x0f)<<8 | uint16(t.Payload[pos+4]))
		end := pos + 5 + esLen
		if end > len(t.Payload) {
			return 0, nil, fmt.Errorf("%w: ES descriptor overruns PMT", ErrMalformedPSI)
		}
		codec := "unknown"
		switch st {
		case StreamTypeH264:
			codec = "h264-synthetic"
		case StreamTypePCMS16LE:
			codec = "pcm_s16le"
		}
		streams = append(streams, PMTStream{StreamType: st, PID: pid, Codec: codec})
		pos = end
	}
	return pcrPID, streams, nil
}
