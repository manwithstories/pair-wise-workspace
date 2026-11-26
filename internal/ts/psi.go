package ts

import (
	"encoding/binary"
	"fmt"

	"avpipe/internal/media"
)

const (
	PATPID           uint16 = 0x0000
	NullPID          uint16 = 0x1fff
	MaxSectionLength        = 1021
)

// PSITable is the validated subset of PAT/PMT used by the indexer.
type PSITable struct {
	PID               uint16
	TableID           byte
	Version           byte
	CurrentNext       bool
	SectionNumber     byte
	LastSectionNumber byte
	TransportID       uint16
	ProgramNumber     uint16
	PMTPID            uint16
	PCRPID            uint16
	VideoPID          uint16
	AudioPID          uint16
	CRC32             uint32
}

type PSIRejection struct {
	PID    uint16          `json:"pid"`
	Offset int64           `json:"offset"`
	Reason media.ErrorCode `json:"reason"`
	Detail string          `json:"detail"`
}

// PSIState retains only committed, CRC-valid tables. A rejected version does
// not replace the previous table.
type PSIState struct {
	PAT          *PSITable            `json:"pat,omitempty"`
	PMTs         map[uint16]*PSITable `json:"pmts"`
	Rejections   []PSIRejection       `json:"rejections"`
	AppliedCount int                  `json:"applied_count"`
}

func NewPSIState() *PSIState { return &PSIState{PMTs: map[uint16]*PSITable{}} }

func (s *PSIState) Reject(pid uint16, offset int64, code media.ErrorCode, format string, args ...any) {
	s.Rejections = append(s.Rejections, PSIRejection{
		PID: pid, Offset: offset, Reason: code, Detail: fmt.Sprintf(format, args...),
	})
}

func (s *PSIState) CRCRejectCount() int {
	n := 0
	for _, r := range s.Rejections {
		if r.Reason == media.CodePSICRC {
			n++
		}
	}
	return n
}

// Apply commits a valid table only when it is current and not older than the
// retained version. A later valid version therefore replaces a rejected one.
func (s *PSIState) Apply(t *PSITable) bool {
	if t == nil || !t.CurrentNext {
		return false
	}
	switch t.TableID {
	case 0x00:
		if s.PAT != nil && versionOlder(t.Version, s.PAT.Version) {
			return false
		}
		s.PAT = t
	case 0x02:
		old := s.PMTs[t.PID]
		if old != nil && versionOlder(t.Version, old.Version) {
			return false
		}
		s.PMTs[t.PID] = t
	default:
		return false
	}
	s.AppliedCount++
	return true
}

func versionOlder(candidate, current byte) bool {
	// 5-bit versions wrap at 31; a small forward distance is newer.
	d := (int(candidate) - int(current)) & 0x1f
	return d > 16
}

// SectionAssembler reassembles PSI sections from packet payloads.
type SectionAssembler struct {
	pending map[uint16][]byte
}

func NewSectionAssembler() *SectionAssembler {
	return &SectionAssembler{pending: map[uint16][]byte{}}
}

// HandlePacket returns every complete valid section found in this packet.
// CRC and length failures are recorded in state and do not mutate old tables.
func (a *SectionAssembler) HandlePacket(p Packet, state *PSIState) []*PSITable {
	if !p.HasPayload || p.PID == NullPID {
		return nil
	}
	payload := p.Payload
	if p.PUSI {
		if len(payload) < 1 {
			state.Reject(p.PID, int64(p.Offset), media.CodePSILength, "PUSI packet missing pointer_field")
			return nil
		}
		pointer := int(payload[0])
		if 1+pointer >= len(payload) {
			state.Reject(p.PID, int64(p.Offset), media.CodePSILength, "pointer_field=%d outside payload", pointer)
			a.pending[p.PID] = nil
			return nil
		}
		payload = payload[1+pointer:]
		a.pending[p.PID] = append([]byte(nil), payload...)
	} else if cur := a.pending[p.PID]; cur != nil {
		a.pending[p.PID] = append(cur, payload...)
	} else {
		return nil
	}

	var tables []*PSITable
	for {
		buf := a.pending[p.PID]
		if len(buf) < 3 {
			return tables
		}
		sectionLength := int(uint16(buf[1]&0x0f)<<8 | uint16(buf[2]))
		if sectionLength > MaxSectionLength {
			state.Reject(p.PID, int64(p.Offset), media.CodePSILength,
				"section_length=%d exceeds max=%d", sectionLength, MaxSectionLength)
			a.pending[p.PID] = nil
			return tables
		}
		total := sectionLength + 3
		if total < 8 || total > 1024 {
			state.Reject(p.PID, int64(p.Offset), media.CodePSILength, "invalid total section size=%d", total)
			a.pending[p.PID] = nil
			return tables
		}
		if len(buf) < total {
			return tables
		}
		section := append([]byte(nil), buf[:total]...)
		if len(buf) > total {
			a.pending[p.PID] = append([]byte(nil), buf[total:]...)
		} else {
			a.pending[p.PID] = nil
		}
		gotCRC := binary.BigEndian.Uint32(section[total-4:])
		wantCRC := CRC32MPEG(section[:total-4])
		if gotCRC != wantCRC {
			state.Reject(p.PID, int64(p.Offset), media.CodePSICRC,
				"CRC mismatch version=%d got=%08x want=%08x", (section[5]>>1)&0x1f, gotCRC, wantCRC)
			continue
		}
		tab, err := ParseSection(p.PID, section)
		if err != nil {
			state.Reject(p.PID, int64(p.Offset), media.CodePSILength, "%v", err)
			continue
		}
		tables = append(tables, tab)
	}
}

func ParseSection(pid uint16, b []byte) (*PSITable, error) {
	if len(b) < 8 {
		return nil, fmt.Errorf("section too short: %d", len(b))
	}
	t := &PSITable{
		PID: pid, TableID: b[0], Version: (b[5] >> 1) & 0x1f,
		CurrentNext: b[5]&0x01 == 1, SectionNumber: b[6], LastSectionNumber: b[7],
		CRC32: binary.BigEndian.Uint32(b[len(b)-4:]),
	}
	switch b[0] {
	case 0x00:
		if len(b) < 16 {
			return nil, fmt.Errorf("PAT too short")
		}
		t.TransportID = binary.BigEndian.Uint16(b[3:5])
		programNumber := binary.BigEndian.Uint16(b[8:10])
		t.ProgramNumber = programNumber
		t.PMTPID = uint16(b[10]&0x1f)<<8 | uint16(b[11])
	case 0x02:
		if len(b) < 14 {
			return nil, fmt.Errorf("PMT too short")
		}
		t.ProgramNumber = binary.BigEndian.Uint16(b[3:5])
		t.PCRPID = uint16(b[8]&0x1f)<<8 | uint16(b[9])
		infoLen := int(uint16(b[10]&0x0f)<<8 | uint16(b[11]))
		pos := 12 + infoLen
		for pos+5 <= len(b)-4 {
			streamType := b[pos]
			epid := uint16(b[pos+1]&0x1f)<<8 | uint16(b[pos+2])
			esLen := int(uint16(b[pos+3]&0x0f)<<8 | uint16(b[pos+4]))
			switch streamType {
			case 0x1b, 0x24:
				t.VideoPID = epid
			case 0x0f, 0x11:
				t.AudioPID = epid
			}
			pos += 5 + esLen
		}
	default:
		return nil, fmt.Errorf("unsupported table_id=0x%02x", b[0])
	}
	return t, nil
}

var crcTable = func() *[256]uint32 {
	var tab [256]uint32
	for i := uint32(0); i < 256; i++ {
		crc := i << 24
		for j := 0; j < 8; j++ {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
		tab[i] = crc
	}
	return &tab
}()

func CRC32MPEG(data []byte) uint32 {
	crc := uint32(0xffffffff)
	for _, b := range data {
		crc = (crc << 8) ^ crcTable[byte(crc>>24)^b]
	}
	return crc
}

func WithCRC(body []byte) []byte {
	out := append([]byte(nil), body...)
	crc := CRC32MPEG(out)
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], crc)
	return append(out, b[:]...)
}
