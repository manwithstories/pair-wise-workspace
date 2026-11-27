package ts

import (
	"fmt"

	"tscheck/internal/model"
)

// CRC32MPEG computes CRC-32/MPEG-2 (poly 0x04C11DB7, init 0xFFFFFFFF,
// no reflection, no final xor) as used by PSI sections.
func CRC32MPEG(b []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, by := range b {
		crc ^= uint32(by) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// StreamInfo describes one elementary stream from the trusted PMT.
type StreamInfo struct {
	PID        uint16
	StreamType uint8
}

// PSITracker assembles PSI sections and maintains the last trusted
// PAT/PMT mapping. Corrupted or illegally-versioned updates are rejected
// and the previous mapping stays in effect.
type PSITracker struct {
	patVersion int            // -1 until a trusted PAT arrives
	pmtVersion map[uint16]int // PMT PID -> trusted version
	pmtPIDs    map[uint16]bool
	streams    []StreamInfo
	buf        map[uint16][]byte
}

// NewPSITracker creates a tracker with no trusted state.
func NewPSITracker() *PSITracker {
	return &PSITracker{
		patVersion: -1,
		pmtVersion: map[uint16]int{},
		pmtPIDs:    map[uint16]bool{},
		buf:        map[uint16][]byte{},
	}
}

// Streams returns the last trusted elementary stream mapping.
func (t *PSITracker) Streams() []StreamInfo {
	out := make([]StreamInfo, len(t.streams))
	copy(out, t.streams)
	return out
}

// IsPSI reports whether pid carries PSI for this tracker (PAT or a known
// PMT PID).
func (t *PSITracker) IsPSI(pid uint16) bool {
	return pid == 0 || t.pmtPIDs[pid]
}

// Feed consumes one PSI packet payload and returns any faults. On faults
// the trusted mapping is left untouched.
func (t *PSITracker) Feed(pi PacketInfo) []model.Fault {
	if !t.IsPSI(pi.PID) || len(pi.Payload) == 0 {
		return nil
	}
	payload := pi.Payload
	if pi.PUSI {
		pointer := int(payload[0])
		if 1+pointer > len(payload) {
			return []model.Fault{{Kind: model.FaultPSICRC, Offset: pi.Offset,
				PID: int(pi.PID), Detail: "pointer field beyond payload"}}
		}
		payload = payload[1+pointer:]
		t.buf[pi.PID] = t.buf[pi.PID][:0]
	}
	t.buf[pi.PID] = append(t.buf[pi.PID], payload...)
	var faults []model.Fault
	for {
		b := t.buf[pi.PID]
		if len(b) < 3 {
			break
		}
		if b[0] == 0xFF { // stuffing
			t.buf[pi.PID] = b[:0]
			break
		}
		secLen := 3 + ((int(b[1]&0x0F) << 8) | int(b[2]))
		if secLen < 4 || secLen > 1024 {
			faults = append(faults, model.Fault{Kind: model.FaultPSICRC,
				Offset: pi.Offset, PID: int(pi.PID),
				Detail: fmt.Sprintf("implausible section length %d", secLen)})
			t.buf[pi.PID] = b[:0]
			break
		}
		if len(b) < secLen {
			break // wait for more packets
		}
		if f := t.section(pi.PID, b[:secLen], pi.Offset); f != nil {
			faults = append(faults, *f)
		}
		t.buf[pi.PID] = b[secLen:]
	}
	return faults
}

// section validates and applies one complete section. It returns a fault
// when the section is rejected.
func (t *PSITracker) section(pid uint16, sec []byte, off int64) *model.Fault {
	if CRC32MPEG(sec) != 0 {
		return &model.Fault{Kind: model.FaultPSICRC, Offset: off, PID: int(pid),
			Detail: "section CRC mismatch; keeping last trusted mapping"}
	}
	version := int(sec[5]>>1) & 0x1F
	currentNext := sec[5]&1 != 0
	if !currentNext {
		return &model.Fault{Kind: model.FaultPSIVersion, Offset: off, PID: int(pid),
			Detail: "section not valid for current use"}
	}
	switch sec[0] {
	case 0x00: // PAT
		if !t.versionOK(t.patVersion, version) {
			return &model.Fault{Kind: model.FaultPSIVersion, Offset: off, PID: int(pid),
				Detail: fmt.Sprintf("PAT version %d -> %d rejected", t.patVersion, version)}
		}
		t.patVersion = version
		t.pmtPIDs = map[uint16]bool{}
		for i := 8; i+4 <= len(sec)-4; i += 4 {
			prog := uint16(sec[i])<<8 | uint16(sec[i+1])
			p := uint16(sec[i+2]&0x1F)<<8 | uint16(sec[i+3])
			if prog != 0 {
				t.pmtPIDs[p] = true
			}
		}
	case 0x02: // PMT
		last, seen := t.pmtVersion[pid]
		if seen && !t.versionOK(last, version) {
			return &model.Fault{Kind: model.FaultPSIVersion, Offset: off, PID: int(pid),
				Detail: fmt.Sprintf("PMT version %d -> %d rejected", last, version)}
		}
		t.pmtVersion[pid] = version
		t.streams = parsePMTStreams(sec)
	}
	return nil
}

// versionOK accepts the first version seen, a retransmission of the
// current version, or an increment by one (mod 32).
func (t *PSITracker) versionOK(last, next int) bool {
	if last < 0 {
		return true
	}
	return next == last || next == (last+1)%32
}

// parsePMTStreams extracts elementary stream entries from a PMT section.
func parsePMTStreams(sec []byte) []StreamInfo {
	if len(sec) < 16 {
		return nil
	}
	progInfo := int(sec[10]&0x0F)<<8 | int(sec[11])
	i := 12 + progInfo
	end := len(sec) - 4
	var out []StreamInfo
	for i+5 <= end {
		st := sec[i]
		epid := uint16(sec[i+1]&0x1F)<<8 | uint16(sec[i+2])
		esInfo := int(sec[i+3]&0x0F)<<8 | int(sec[i+4])
		out = append(out, StreamInfo{PID: epid, StreamType: st})
		i += 5 + esInfo
	}
	return out
}
