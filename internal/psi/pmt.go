package psi

import (
	"encoding/binary"
	"errors"
)

// Stream is one elementary stream entry of a PMT.
type Stream struct {
	Type         uint8
	PID          uint16
	Lang         string // ISO 639 descriptor 0x0A, empty if absent
	Registration string // registration descriptor 0x05 format identifier
}

// PMT is a parsed Program Map Table section.
type PMT struct {
	Program uint16
	Version int
	Current bool
	PCRPID  uint16
	Streams []Stream
}

var errNotPMT = errors.New("psi: not a PMT section")

// ParsePMT parses a CRC-valid PMT section (table_id 0x02).
func ParsePMT(sec []byte) (*PMT, error) {
	if len(sec) < 16 || sec[0] != 0x02 {
		return nil, errNotPMT
	}
	if sec[1]&0x80 == 0 {
		return nil, errNotPMT
	}
	p := &PMT{
		Program: binary.BigEndian.Uint16(sec[3:5]),
		Version: int(sec[5]>>1) & 0x1F,
		Current: sec[5]&0x01 != 0,
		PCRPID:  uint16(sec[8]&0x1F)<<8 | uint16(sec[9]),
	}
	progInfoLen := int(sec[10]&0x0F)<<8 | int(sec[11])
	pos := 12 + progInfoLen
	end := len(sec) - 4
	if pos > end {
		return nil, errNotPMT
	}
	for pos+5 <= end {
		s := Stream{
			Type: sec[pos],
			PID:  uint16(sec[pos+1]&0x1F)<<8 | uint16(sec[pos+2]),
		}
		esInfoLen := int(sec[pos+3]&0x0F)<<8 | int(sec[pos+4])
		descEnd := pos + 5 + esInfoLen
		if descEnd > end {
			break
		}
		parseStreamDescriptors(sec[pos+5:descEnd], &s)
		p.Streams = append(p.Streams, s)
		pos = descEnd
	}
	return p, nil
}

func parseStreamDescriptors(b []byte, s *Stream) {
	for len(b) >= 2 {
		tag, l := b[0], int(b[1])
		if len(b) < 2+l {
			return
		}
		d := b[2 : 2+l]
		switch tag {
		case 0x0A: // ISO 639 language
			if len(d) >= 3 {
				s.Lang = string(d[:3])
			}
		case 0x05: // registration
			if len(d) >= 4 {
				s.Registration = string(d[:4])
			}
		}
		b = b[2+l:]
	}
}
