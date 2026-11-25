// Package fixture synthesizes deterministic MPEG-TS live streams for demos and
// tests. Everything is generated in-process from a fixed seed; no external
// sample files are read.
package fixture

import (
	"streamcut/internal/ts"
)

// Well-known PIDs shared by the generator and its consumers.
const (
	PMTPID  uint16 = 0x1000
	VideoPID uint16 = 0x0100
	AudioPID uint16 = 0x0101
)

// Muxer emits 188-byte TS packets with per-PID continuity counters. It
// supports the controlled faults tests need: CC jumps and PCR injection.
type Muxer struct {
	cc       map[uint16]uint8
	ccJump   map[uint16]bool // PIDs whose next packet must skip the CC
}

// NewMuxer returns a muxer with all counters anchored at zero.
func NewMuxer() *Muxer {
	return &Muxer{cc: map[uint16]uint8{}, ccJump: map[uint16]bool{}}
}

// ForceCCJump makes the next packet on pid skip three continuity values.
func (m *Muxer) ForceCCJump(pid uint16) { m.ccJump[pid] = true }

func (m *Muxer) nextCC(pid uint16) uint8 {
	c := m.cc[pid]
	if m.ccJump[pid] {
		c = (c + 3) & 0x0F
		m.ccJump[pid] = false
	}
	m.cc[pid] = c + 1
	return c
}

// PacketizePSI wraps one PSI section in payload-only packets, PUSI with a
// zero pointer field on the first one.
func (m *Muxer) PacketizePSI(pid uint16, section []byte) [][]byte {
	var out [][]byte
	rest := section
	first := true
	for len(rest) > 0 {
		pkt := make([]byte, ts.PacketSize)
		pkt[0] = 0x47
		pkt[1] = byte(pid>>8) | 0x10
		pkt[2] = byte(pid)
		off := 4
		if first {
			pkt[1] |= 0x40
			pkt[4] = 0x00 // pointer field
			off = 5
			first = false
		}
		n := copy(pkt[off:], rest)
		rest = rest[n:]
		for i := off + n; i < ts.PacketSize; i++ {
			pkt[i] = 0xFF
		}
		pkt[3] = 0x10 | m.nextCC(pid)
		out = append(out, pkt)
	}
	return out
}

// PacketizePES splits a complete PES packet across TS packets. When pcr is
// non-nil it is placed in the first packet's adaptation field (27 MHz units).
func (m *Muxer) PacketizePES(pid uint16, pes []byte, pcr *uint64, randomAccess bool) [][]byte {
	var out [][]byte
	rest := pes
	first := true
	for len(rest) > 0 {
		pkt := make([]byte, ts.PacketSize)
		pkt[0] = 0x47
		pkt[1] = byte(pid>>8) | 0x10
		pkt[2] = byte(pid)
		off := 4
		if first {
			pkt[1] |= 0x40 // PUSI: PES packet starts here
			off = m.writeAdaptation(pkt, pcr, randomAccess)
			first = false
		}
		n := copy(pkt[off:], rest)
		rest = rest[n:]
		for i := off + n; i < ts.PacketSize; i++ {
			pkt[i] = 0xFF
		}
		pkt[3] = 0x10 | m.nextCC(pid)
		out = append(out, pkt)
	}
	return out
}

// writeAdaptation reserves an adaptation field on the first PES packet when a
// PCR or random-access indicator is needed, returning the payload offset.
func (m *Muxer) writeAdaptation(pkt []byte, pcr *uint64, randomAccess bool) int {
	if pcr == nil && !randomAccess {
		return 4
	}
	var field []byte
	flags := byte(0)
	if randomAccess {
		flags |= 0x40
	}
	if pcr != nil {
		flags |= 0x10
		b := *pcr
		base, ext := b/300, b%300
		field = make([]byte, 0, 7)
		field = append(field, flags)
		field = append(field,
			byte(base>>25), byte(base>>17), byte(base>>9),
			byte(base>>1), byte(base<<1)|byte(ext>>8), byte(ext))
	} else {
		field = []byte{flags}
	}
	pkt[3] |= 0x20 // adaptation + payload
	pkt[4] = byte(len(field))
	copy(pkt[5:], field)
	return 5 + len(field)
}

// PCROnlyPacket emits an adaptation-only packet carrying pcr, used to keep the
// program clock alive between PES events.
func (m *Muxer) PCROnlyPacket(pid uint16, pcr uint64) []byte {
	pkt := make([]byte, ts.PacketSize)
	pkt[0] = 0x47
	pkt[1] = byte(pid>>8) | 0x10
	pkt[2] = byte(pid)
	pcrPtr := pcr
	off := m.writeAdaptation(pkt, &pcrPtr, false)
	for i := off; i < ts.PacketSize; i++ {
		pkt[i] = 0xFF
	}
	pkt[3] = 0x20 | m.nextCC(pid)
	return pkt
}
