package psi

import (
	"encoding/binary"
	"errors"
)

// PATEntry maps a program number to its PMT PID.
type PATEntry struct {
	Program uint16
	PMTPID  uint16
}

// PAT is a parsed Program Association Table section.
type PAT struct {
	Version int
	Current bool
	Entries []PATEntry
}

var errNotPAT = errors.New("psi: not a PAT section")

// ParsePAT parses a CRC-valid PAT section (table_id 0x00).
func ParsePAT(sec []byte) (*PAT, error) {
	if len(sec) < 12 || sec[0] != 0x00 {
		return nil, errNotPAT
	}
	if sec[1]&0x80 == 0 {
		return nil, errNotPAT
	}
	p := &PAT{
		Version: int(sec[5]>>1) & 0x1F,
		Current: sec[5]&0x01 != 0,
	}
	body := sec[8 : len(sec)-4]
	for len(body) >= 4 {
		prog := binary.BigEndian.Uint16(body)
		pid := uint16(body[2]&0x1F)<<8 | uint16(body[3])
		if prog != 0 { // program 0 is the network PID, not a PMT
			p.Entries = append(p.Entries, PATEntry{Program: prog, PMTPID: pid})
		}
		body = body[4:]
	}
	return p, nil
}
