package ts

import (
	"encoding/binary"

	"tsqc/internal/clock"
)

// ParsePAT decodes a PAT section body.
func ParsePAT(s Section) (*PAT, error) {
	if s.TableID != 0x00 {
		return nil, clock.RecoverableError(clock.CodePSIInvalid, "ts.ParsePAT", "table id %#x", s.TableID)
	}
	body := s.Body
	if len(body)%4 != 0 {
		return nil, clock.RecoverableError(clock.CodePSIInvalid, "ts.ParsePAT", "body length %d", len(body))
	}
	p := &PAT{Version: s.Version, Programs: make(map[uint16]uint16)}
	for i := 0; i+4 <= len(body); i += 4 {
		prog := binary.BigEndian.Uint16(body[i:])
		pid := binary.BigEndian.Uint16(body[i+2:]) & 0x1fff
		if prog == 0 {
			continue // network PID
		}
		p.Programs[prog] = pid
	}
	if len(p.Programs) == 0 {
		return nil, clock.RecoverableError(clock.CodePSIInvalid, "ts.ParsePAT", "no programs")
	}
	return p, nil
}

// ParsePMT decodes a PMT section body.
func ParsePMT(s Section) (*PMT, error) {
	if s.TableID != 0x02 {
		return nil, clock.RecoverableError(clock.CodePSIInvalid, "ts.ParsePMT", "table id %#x", s.TableID)
	}
	if len(s.Body) < 4 {
		return nil, clock.RecoverableError(clock.CodePSIInvalid, "ts.ParsePMT", "body length %d", len(s.Body))
	}
	p := &PMT{Program: s.Ext, Version: s.Version}
	p.PCRPID = binary.BigEndian.Uint16(s.Body[0:]) & 0x1fff
	infoLen := int(s.Body[2]&0x0f)<<8 | int(s.Body[3])
	off := 4
	if off+infoLen > len(s.Body) {
		return nil, clock.RecoverableError(clock.CodePSIInvalid, "ts.ParsePMT", "program info overruns")
	}
	p.ProgramInfo = append([]byte(nil), s.Body[off:off+infoLen]...)
	off += infoLen
	for off+5 <= len(s.Body) {
		st := s.Body[off]
		pid := binary.BigEndian.Uint16(s.Body[off+1:]) & 0x1fff
		esLen := int(s.Body[off+3]&0x0f)<<8 | int(s.Body[off+4])
		if off+5+esLen > len(s.Body) {
			return nil, clock.RecoverableError(clock.CodePSIInvalid, "ts.ParsePMT", "stream descriptor overruns")
		}
		p.Streams = append(p.Streams, Stream{Type: st, PID: pid, Private: append([]byte(nil), s.Body[off+5:off+5+esLen]...)})
		off += 5 + esLen
	}
	if len(p.Streams) == 0 {
		return nil, clock.RecoverableError(clock.CodePSIInvalid, "ts.ParsePMT", "no streams")
	}
	return p, nil
}

// TableManager applies PAT/PMT updates atomically: a new version only becomes
// active once its section has a valid CRC and the current_next_indicator is
// set. Corrupt sections never disturb the active tables.
type TableManager struct {
	pat        *PAT
	pmt        map[uint16]*PMT
	patVer     int
	pmtVer     map[uint16]int
	LastSwitch string
	Rejected   int
}

// NewTableManager builds an empty table manager.
func NewTableManager() *TableManager {
	return &TableManager{patVer: -1, pmtVer: make(map[uint16]int), pmt: make(map[uint16]*PMT)}
}

// Apply ingests one validated section. It returns true when the active table
// changed. CRC failures are rejected upstream by ParseSection.
func (t *TableManager) Apply(s Section) (bool, error) {
	if !s.Current {
		t.Rejected++
		return false, clock.RecoverableError(clock.CodePSIVersion, "ts.TableManager", "current_next_indicator=0 for table %#x", s.TableID)
	}
	switch s.TableID {
	case 0x00:
		p, err := ParsePAT(s)
		if err != nil {
			t.Rejected++
			return false, err
		}
		if int(s.Version) == t.patVer {
			return false, nil
		}
		t.pat = p
		t.patVer = int(s.Version)
		t.LastSwitch = "pat"
		return true, nil
	case 0x02:
		p, err := ParsePMT(s)
		if err != nil {
			t.Rejected++
			return false, err
		}
		if ver, ok := t.pmtVer[s.Ext]; ok && int(s.Version) == ver {
			return false, nil
		}
		t.pmt[s.Ext] = p
		t.pmtVer[s.Ext] = int(s.Version)
		t.LastSwitch = "pmt"
		return true, nil
	}
	return false, nil
}

// PAT returns the active PAT, or nil.
func (t *TableManager) PAT() *PAT { return t.pat }

// PMT returns the active PMT for a program, or nil.
func (t *TableManager) PMT(program uint16) *PMT { return t.pmt[program] }

// Programs returns the active program numbers in ascending order.
func (t *TableManager) Programs() []uint16 {
	if t.pat == nil {
		return nil
	}
	var out []uint16
	for prog := range t.pat.Programs {
		out = append(out, prog)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// ActivePMT returns the PMT of the lowest-numbered program.
func (t *TableManager) ActivePMT() *PMT {
	progs := t.Programs()
	if len(progs) == 0 {
		return nil
	}
	return t.pmt[progs[0]]
}
