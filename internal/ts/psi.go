package ts

import (
	"fmt"

	"tsbatch/internal/fault"
)

// MaxSectionSize bounds a single PSI section (ETSI allows up to 4096).
const MaxSectionSize = 4096

// Section is one fully reassembled PSI section.
type Section struct {
	TableID    uint8
	Version    uint8
	Current    bool
	Number     uint8
	Last       uint8
	TableIDExt uint16 // e.g. transport_stream_id / program_number
	Raw        []byte // complete section including header and CRC
	// EndPacket is the packet sequence number at which the section
	// completed — the point where a version update takes effect.
	EndPacket uint64
}

// Assembler reassembles PSI sections of one PID across packet payloads,
// honouring pointer_field and multiple sections per packet.
type Assembler struct {
	buf         []byte
	want        int // total expected length, 0 until the header is known
	startPacket uint64
}

// Push consumes one payload. pusi marks a payload_unit_start, in which
// case the first byte is pointer_field. Completed sections are returned;
// their EndPacket is set to packetIdx.
func (a *Assembler) Push(payload []byte, pusi bool, packetIdx uint64) []Section {
	var out []Section
	if pusi {
		if len(payload) == 0 {
			return nil
		}
		pf := int(payload[0])
		payload = payload[1:]
		if pf > len(payload) {
			a.reset()
			return nil
		}
		// Bytes before the pointer complete the previous section.
		if pf > 0 && len(a.buf) > 0 {
			out = append(out, a.feed(payload[:pf], packetIdx)...)
		} else if len(a.buf) > 0 {
			a.reset() // dangling partial section superseded
		}
		payload = payload[pf:]
	}
	out = append(out, a.feed(payload, packetIdx)...)
	return out
}

func (a *Assembler) feed(payload []byte, packetIdx uint64) []Section {
	var out []Section
	for len(payload) > 0 {
		if len(a.buf) == 0 {
			if payload[0] == 0xFF { // stuffing between sections
				return out
			}
			a.startPacket = packetIdx
			a.want = 0
		}
		take := len(payload)
		if a.want > 0 && len(a.buf)+take > a.want {
			take = a.want - len(a.buf)
		}
		a.buf = append(a.buf, payload[:take]...)
		payload = payload[take:]
		if a.want == 0 && len(a.buf) >= 3 {
			a.want = 3 + (int(a.buf[1]&0x0F)<<8 | int(a.buf[2]))
			if a.want < 12 || a.want > MaxSectionSize {
				a.reset()
				continue
			}
		}
		if a.want > 0 && len(a.buf) == a.want {
			if s, ok := parseSection(a.buf, packetIdx); ok {
				out = append(out, s)
			}
			a.reset()
		}
	}
	return out
}

func (a *Assembler) reset() { a.buf = a.buf[:0]; a.want = 0 }

// Pending reports a partially assembled section and where it started.
func (a *Assembler) Pending() (startPacket uint64, bytes int, ok bool) {
	if len(a.buf) == 0 {
		return 0, 0, false
	}
	return a.startPacket, len(a.buf), true
}

func parseSection(raw []byte, endPacket uint64) (Section, bool) {
	if len(raw) < 12 { // header(3) + ext(5) + CRC(4) minimum
		return Section{}, false
	}
	s := Section{
		TableID:    raw[0],
		TableIDExt: uint16(raw[3])<<8 | uint16(raw[4]),
		Version:    raw[5] >> 1 & 0x1F,
		Current:    raw[5]&0x01 != 0,
		Number:     raw[6],
		Last:       raw[7],
		Raw:        raw,
		EndPacket:  endPacket,
	}
	return s, true
}

// CRC32MPEG computes CRC-32/MPEG-2 (poly 0x04C11DB7, init 0xFFFFFFFF,
// no reflection, no final xor) as used by PSI sections.
func CRC32MPEG(data []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, b := range data {
		crc ^= uint32(b) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// ValidCRC reports whether the section's trailing CRC matches.
func ValidCRC(raw []byte) bool {
	return len(raw) >= 4 && CRC32MPEG(raw) == 0
}

// TableEvent records an accepted PSI version update.
type TableEvent struct {
	TableID  uint8
	Version  uint8
	AtPacket uint64 // packet where the new version took effect
}

// TableTracker validates sections of one PID against the last known-good
// table. Bad sections never overwrite the last valid table.
type TableTracker struct {
	version uint8
	have    bool
	Updates []TableEvent
	Faults  []fault.Fault
}

// HasTable reports whether a valid baseline table exists.
func (t *TableTracker) HasTable() bool { return t.have }

// Update validates sec and returns true when it was accepted.
func (t *TableTracker) Update(sec Section, pid uint16, mediaTime int64) bool {
	mk := func(cat fault.Category, detail string) {
		t.Faults = append(t.Faults, fault.Fault{
			Category: cat, PID: pid, PacketIndex: sec.EndPacket,
			MediaTime: mediaTime, Recoverable: true, Detail: detail,
		})
	}
	if !ValidCRC(sec.Raw) {
		mk(fault.CatPSICRC, fmt.Sprintf("table_id=0x%02x crc mismatch", sec.TableID))
		return false
	}
	if t.have {
		next := (t.version + 1) & 0x1F
		if sec.Version != t.version && sec.Version != next {
			mk(fault.CatPSIRollback, fmt.Sprintf("table_id=0x%02x version %d -> %d rejected",
				sec.TableID, t.version, sec.Version))
			return false
		}
		if sec.Version == t.version {
			return true // repeat of the current table
		}
	}
	if !t.have {
		// The first table seen is the baseline, not an update.
		t.version, t.have = sec.Version, true
		return true
	}
	if sec.Version != t.version {
		t.Updates = append(t.Updates, TableEvent{
			TableID: sec.TableID, Version: sec.Version, AtPacket: sec.EndPacket,
		})
	}
	t.version = sec.Version
	return true
}

// PMTEntry is one elementary stream of a program.
type PMTEntry struct {
	StreamType uint8
	PID        uint16
}

// ParsePAT maps program numbers to PMT PIDs. sec must be CRC-valid.
func ParsePAT(sec Section) map[uint16]uint16 {
	out := map[uint16]uint16{}
	body := sec.Raw[8 : len(sec.Raw)-4]
	for i := 0; i+4 <= len(body); i += 4 {
		prog := uint16(body[i])<<8 | uint16(body[i+1])
		pid := uint16(body[i+2]&0x1F)<<8 | uint16(body[i+3])
		if prog != 0 {
			out[prog] = pid
		}
	}
	return out
}

// ParsePMT lists the elementary streams of a program.
func ParsePMT(sec Section) (pcrPID uint16, entries []PMTEntry) {
	pcrPID = uint16(sec.Raw[8]&0x1F)<<8 | uint16(sec.Raw[9])
	progInfoLen := int(sec.Raw[10]&0x0F)<<8 | int(sec.Raw[11])
	body := sec.Raw[12+progInfoLen : len(sec.Raw)-4]
	for i := 0; i+5 <= len(body); {
		esInfoLen := int(body[i+3]&0x0F)<<8 | int(body[i+4])
		entries = append(entries, PMTEntry{
			StreamType: body[i],
			PID:        uint16(body[i+1]&0x1F)<<8 | uint16(body[i+2]),
		})
		i += 5 + esInfoLen
	}
	return pcrPID, entries
}
