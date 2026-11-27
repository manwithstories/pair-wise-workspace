package ts

import (
	"encoding/binary"

	"tsqc/internal/clock"
)

// CRC32 computes the MPEG-2 PSI CRC (polynomial 0x04C11DB7, no reflection).
func CRC32(b []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, c := range b {
		crc ^= uint32(c) << 24
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

// Section is one complete PSI/SI section including its CRC.
type Section struct {
	TableID    uint8
	Syntax     bool
	Length     int
	Ext        uint16
	Version    uint8
	Current    bool
	SectionNum uint8
	LastNum    uint8
	Body       []byte // bytes after the 8-byte long header up to (excluding) CRC
	Raw        []byte
}

// Stream describes one elementary stream in a PMT.
type Stream struct {
	Type    uint8
	PID     uint16
	Private []byte
}

// PAT is a parsed program association table.
type PAT struct {
	Version  uint8
	Programs map[uint16]uint16 // program number -> PMT PID
}

// PMT is a parsed program map table.
type PMT struct {
	Program     uint16
	Version     uint8
	PCRPID      uint16
	Streams     []Stream
	ProgramInfo []byte
}

// SectionAssembler reassembles sections spanning TS packets, including the
// pointer_field continuation rules.
type SectionAssembler struct {
	buf      []byte
	need     int
	started  bool
	Sections int
	Errors   int
}

// Reset drops any partial section.
func (a *SectionAssembler) Reset() {
	a.buf = nil
	a.need = 0
	a.started = false
}

// Feed consumes one TS payload and returns all sections completed by it.
func (a *SectionAssembler) Feed(payload []byte, pusi bool) ([]Section, error) {
	var out []Section
	var firstErr error
	if len(payload) == 0 {
		return nil, nil
	}
	rest := payload
	if pusi {
		ptr := int(payload[0])
		if 1+ptr > len(payload) {
			a.Reset()
			a.Errors++
			return nil, clock.RecoverableError(clock.CodePSIInvalid, "ts.SectionAssembler", "pointer_field %d overruns payload %d", ptr, len(payload))
		}
		if a.started && a.need > 0 {
			secs, err := a.append(payload[1 : 1+ptr])
			out = append(out, secs...)
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		a.Reset()
		rest = payload[1+ptr:]
	}
	secs, err := a.append(rest)
	out = append(out, secs...)
	if err != nil && firstErr == nil {
		firstErr = err
	}
	return out, firstErr
}

func (a *SectionAssembler) append(b []byte) ([]Section, error) {
	var out []Section
	for len(b) > 0 {
		if !a.started {
			if b[0] == 0xFF {
				return out, nil
			}
			if len(b) < 3 {
				a.buf = append(a.buf, b...)
				a.started = true
				a.need = 3
				return out, nil
			}
			length := int(b[1]&0x0f)<<8 | int(b[2])
			a.need = 3 + length
			if a.need < 3 || a.need > 1024 {
				a.Errors++
				a.Reset()
				return out, clock.RecoverableError(clock.CodePSIInvalid, "ts.SectionAssembler", "implausible section length %d", length)
			}
			a.started = true
			a.buf = a.buf[:0]
		}
		take := a.need - len(a.buf)
		if take > len(b) {
			take = len(b)
		}
		a.buf = append(a.buf, b[:take]...)
		b = b[take:]
		if len(a.buf) < a.need {
			return out, nil
		}
		sec, err := ParseSection(a.buf)
		a.started = false
		a.buf = a.buf[:0]
		if err != nil {
			a.Errors++
			return out, err
		}
		a.Sections++
		out = append(out, sec)
	}
	return out, nil
}

// ParseSection validates length and CRC and decodes the long header.
func ParseSection(raw []byte) (Section, error) {
	var s Section
	if len(raw) < 12 {
		return s, clock.RecoverableError(clock.CodePSIInvalid, "ts.ParseSection", "section too short: %d", len(raw))
	}
	s.Raw = append([]byte(nil), raw...)
	s.TableID = raw[0]
	s.Syntax = raw[1]&0x80 != 0
	length := int(raw[1]&0x0f)<<8 | int(raw[2])
	if 3+length != len(raw) {
		return s, clock.RecoverableError(clock.CodePSIInvalid, "ts.ParseSection", "length field %d != buffer %d", 3+length, len(raw))
	}
	if !s.Syntax {
		return s, clock.RecoverableError(clock.CodePSIInvalid, "ts.ParseSection", "table %#x is not a long section", s.TableID)
	}
	s.Ext = binary.BigEndian.Uint16(raw[3:5])
	s.Version = raw[5] >> 1 & 0x1f
	s.Current = raw[5]&0x01 != 0
	s.SectionNum = raw[6]
	s.LastNum = raw[7]
	s.Length = length
	s.Body = raw[8 : 3+length-4]
	if got, want := binary.BigEndian.Uint32(raw[len(raw)-4:]), CRC32(raw[:len(raw)-4]); got != want {
		return s, clock.RecoverableError(clock.CodePSIInvalid, "ts.ParseSection", "CRC %08x != %08x", got, want)
	}
	return s, nil
}
