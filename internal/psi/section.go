package psi

const (
	// MaxSectionLength is the upper bound of the section_length field.
	MaxSectionLength = 4093
	// MaxSectionSize is header (3) + section_length + CRC (4).
	MaxSectionSize = 3 + MaxSectionLength
)

// Counters tracks assembly-level error statistics.
type Counters struct {
	CRCErrors    uint64 `json:"crc_errors"`
	LengthErrors uint64 `json:"length_errors"`
	Overflows    uint64 `json:"overflows"`
}

// Section is a fully reassembled, CRC-valid PSI section (CRC included).
type Section struct {
	PID    uint16
	Offset int64 // byte offset of the packet that completed the section
	Raw    []byte
}

// TableID returns the table_id of the section.
func (s *Section) TableID() byte { return s.Raw[0] }

// Assembler reassembles PSI sections for one PID.
type Assembler struct {
	pid  uint16
	buf  []byte // bytes of the section currently being assembled
	need int    // total expected length of buf, 0 while header incomplete
}

// NewAssembler creates an Assembler for one PID.
func NewAssembler(pid uint16) *Assembler { return &Assembler{pid: pid} }

// Feed consumes one TS payload and returns any completed sections.
// completeOff is the byte offset of the packet carrying this payload.
func (a *Assembler) Feed(payload []byte, pusi bool, off int64, c *Counters) []Section {
	var out []Section
	if pusi {
		if len(payload) == 0 {
			return nil
		}
		pointer := int(payload[0])
		rest := payload[1:]
		if pointer > len(rest) {
			// pointer_field out of range: drop residual and the new section.
			c.LengthErrors++
			a.reset()
			return nil
		}
		// Complete any pending section with the pre-pointer residual.
		if len(a.buf) > 0 {
			out = append(out, a.appendBytes(rest[:pointer], off, c)...)
		}
		a.reset()
		rest = rest[pointer:]
		out = append(out, a.drain(rest, off, c)...)
		return out
	}
	if len(a.buf) == 0 {
		return nil // no section in progress; ignore
	}
	return a.appendBytes(payload, off, c)
}

// appendBytes adds continuation bytes to the pending section.
func (a *Assembler) appendBytes(data []byte, off int64, c *Counters) []Section {
	if len(a.buf) == 0 {
		return nil
	}
	take := len(data)
	if a.need > 0 && len(a.buf)+take > a.need {
		take = a.need - len(a.buf)
	}
	a.buf = append(a.buf, data[:take]...)
	if a.need == 0 && len(a.buf) >= 3 {
		sl := int(a.buf[1]&0x0F)<<8 | int(a.buf[2])
		if sl > MaxSectionLength {
			c.LengthErrors++
			a.reset()
			return nil
		}
		a.need = 3 + sl
		if len(a.buf) > a.need {
			a.buf = a.buf[:a.need]
		}
	}
	if a.need > 0 && len(a.buf) == a.need {
		sec := a.buf
		a.reset()
		if !checkCRC(sec) {
			c.CRCErrors++
			return nil
		}
		return []Section{{PID: a.pid, Offset: off, Raw: sec}}
	}
	return nil
}

// drain processes whole sections that start at the beginning of data,
// stopping at 0xFF stuffing.
func (a *Assembler) drain(data []byte, off int64, c *Counters) []Section {
	var out []Section
	for len(data) >= 3 {
		if data[0] == 0xFF {
			break // stuffing
		}
		sl := int(data[1]&0x0F)<<8 | int(data[2])
		if sl > MaxSectionLength {
			c.LengthErrors++
			return out
		}
		total := 3 + sl
		if len(data) < total {
			// Section spans packets: keep the remainder pending.
			a.buf = append(a.buf[:0], data...)
			a.need = total
			return out
		}
		sec := data[:total]
		if checkCRC(sec) {
			out = append(out, Section{PID: a.pid, Offset: off, Raw: sec})
		} else {
			c.CRCErrors++
		}
		data = data[total:]
	}
	return out
}

func (a *Assembler) reset() {
	a.buf = a.buf[:0]
	a.need = 0
}

func checkCRC(sec []byte) bool {
	return len(sec) >= 4 && CRC32MPEG(sec) == 0
}
