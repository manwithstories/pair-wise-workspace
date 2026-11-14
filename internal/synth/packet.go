package synth

import (
	"encoding/binary"
	"io"

	"tsaudit/internal/tspipe"
)

type packetWriter struct {
	w       io.Writer
	buf     [tspipe.PacketSize]byte
	cc      map[uint16]uint8
	written int64
	bytes   int64
}

func newPacketWriter(w io.Writer) *packetWriter {
	return &packetWriter{w: w, cc: make(map[uint16]uint8)}
}

func (p *packetWriter) payloadPacket(pid uint16, pusi bool, payload []byte) []byte {
	cc := p.cc[pid]
	p.cc[pid] = (cc + 1) & 0x0f
	p.buf[0] = 0x47
	b := byte(pid >> 8)
	if pusi {
		b |= 0x40
	}
	p.buf[1], p.buf[2] = b, byte(pid)
	p.buf[3] = 0x10 | cc
	n := copy(p.buf[4:], payload)
	for i := 4 + n; i < tspipe.PacketSize; i++ {
		p.buf[i] = 0xff
	}
	return p.buf[:]
}

func (p *packetWriter) pcrPacket(pid uint16, base uint64) []byte {
	p.buf[0] = 0x47
	p.buf[1], p.buf[2] = byte(pid>>8), byte(pid)
	p.buf[3] = 0x20 | p.cc[pid] // Adaptation-only packets do not increment CC.
	p.buf[4] = 183
	p.buf[5] = 0x10
	p.buf[6] = byte(base >> 25)
	p.buf[7] = byte(base >> 17)
	p.buf[8] = byte(base >> 9)
	p.buf[9] = byte(base >> 1)
	p.buf[10] = byte(base<<7) | 0x7e
	p.buf[11] = 0
	for i := 12; i < tspipe.PacketSize; i++ {
		p.buf[i] = 0xff
	}
	return p.buf[:]
}

func (p *packetWriter) nullPacket() []byte {
	p.buf[0] = 0x47
	p.buf[1], p.buf[2] = 0x1f, 0xff
	p.buf[3] = 0x10
	for i := 4; i < tspipe.PacketSize; i++ {
		p.buf[i] = 0xff
	}
	return p.buf[:]
}

func (p *packetWriter) put(raw []byte) error {
	n, err := p.w.Write(raw)
	if err != nil {
		return err
	}
	p.written++
	p.bytes += int64(n)
	return nil
}

func makePES(def streamDef, sequence uint64) []byte {
	const dataLen = 170
	out := make([]byte, 14+dataLen)
	out[0], out[1], out[2] = 0x00, 0x00, 0x01
	out[3] = def.StreamID
	binary.BigEndian.PutUint16(out[4:6], uint16(3+5+dataLen))
	out[6], out[7], out[8] = 0x80, 0x80, 0x05
	pts := encodePTS(sequence * 90)
	copy(out[9:14], pts[:])
	var seed [8]byte
	binary.BigEndian.PutUint64(seed[:], sequence)
	for i := 14; i < len(out); i++ {
		out[i] = seed[i%len(seed)] ^ byte(def.PID) ^ byte(i)
	}
	return out
}

func encodePTS(value uint64) [5]byte {
	value &= 1<<33 - 1
	return [5]byte{
		0x21 | byte(value>>29)&0x0e,
		byte(value >> 22),
		byte(value>>14)&0xfe | 1,
		byte(value >> 7),
		byte(value<<1)&0xfe | 1,
	}
}
