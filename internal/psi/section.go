package psi

import "fmt"

const maxSectionSize = 4096

type Section struct {
	PID         uint16
	PacketIndex int64
	Data        []byte
}

type SectionStats struct {
	Sections          int64
	CRCOk             int64
	CRCErrors         int64
	TruncatedSections int64
}

type Assembler struct {
	pending map[uint16][]byte
	stats   SectionStats
}

func NewAssembler() *Assembler {
	return &Assembler{pending: make(map[uint16][]byte)}
}

func (a *Assembler) Push(pid uint16, payload []byte, pusi bool, packetIndex int64) ([]Section, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	if !pusi {
		if len(a.pending[pid]) == 0 {
			return nil, nil
		}
		return a.feed(pid, payload, false, packetIndex)
	}

	pointer := int(payload[0])
	if pointer > len(payload)-1 {
		return nil, fmt.Errorf("psi: pid %#x pointer_field %d exceeds payload", pid, pointer)
	}
	var out []Section
	if len(a.pending[pid]) > 0 {
		sections, err := a.feed(pid, payload[1:1+pointer], false, packetIndex)
		if err != nil {
			return out, err
		}
		out = append(out, sections...)
		if len(a.pending[pid]) > 0 {
			a.stats.TruncatedSections++
			delete(a.pending, pid)
		}
	}
	sections, err := a.feed(pid, payload[1+pointer:], true, packetIndex)
	if err != nil {
		return out, err
	}
	return append(out, sections...), nil
}

func (a *Assembler) feed(pid uint16, chunk []byte, allowStart bool, packetIndex int64) ([]Section, error) {
	var sections []Section
	for len(chunk) > 0 || len(a.pending[pid]) > 0 {
		pending := a.pending[pid]
		if len(pending) == 0 {
			if !allowStart || allStuffing(chunk) {
				break
			}
			pending = make([]byte, 0, minInt(len(chunk), 256))
		}
		if len(pending) < 3 {
			take := minInt(3-len(pending), len(chunk))
			pending = append(pending, chunk[:take]...)
			chunk = chunk[take:]
			if len(pending) < 3 {
				a.pending[pid] = pending
				break
			}
		}
		total := 3 + (int(pending[1]&0x0f)<<8 | int(pending[2]))
		if total > maxSectionSize {
			delete(a.pending, pid)
			return sections, fmt.Errorf("psi: pid %#x section length %d exceeds limit", pid, total)
		}
		need := total - len(pending)
		if need > 0 {
			take := minInt(need, len(chunk))
			pending = append(pending, chunk[:take]...)
			chunk = chunk[take:]
		}
		if len(pending) < total {
			a.pending[pid] = pending
			break
		}
		sec := Section{PID: pid, PacketIndex: packetIndex, Data: pending}
		delete(a.pending, pid)
		a.stats.Sections++
		if CRC32MPEG(sec.Data) != 0 {
			a.stats.CRCErrors++
		} else {
			a.stats.CRCOk++
			sections = append(sections, sec)
		}
		allowStart = true
	}
	return sections, nil
}

func allStuffing(data []byte) bool {
	for _, b := range data {
		if b != 0xff {
			return false
		}
	}
	return true
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (a *Assembler) Finish() {
	for pid := range a.pending {
		a.stats.TruncatedSections++
		delete(a.pending, pid)
	}
}

func (a *Assembler) Stats() SectionStats {
	return a.stats
}

// CRC32MPEG computes the MPEG-2 PSI CRC. A complete section with a valid CRC
// evaluates to zero.
func CRC32MPEG(data []byte) uint32 {
	crc := uint32(0xffffffff)
	for _, b := range data {
		crc ^= uint32(b) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
