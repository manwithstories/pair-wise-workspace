// Package ts parses 188-byte MPEG transport stream packets, PSI tables and
// the H.264/AAC elementary streams carried inside them.
package ts

import (
	"tsqc/internal/clock"
)

const (
	// PacketSize is the fixed MPEG-TS packet length.
	PacketSize = 188
	// SyncByte begins every TS packet.
	SyncByte = 0x47

	PIDPAT  = 0x0000
	PIDNull = 0x1FFF

	StreamH264 = 0x1B
	StreamAAC  = 0x0F

	adaptationPayloadOnly = 0x01
	adaptationOnly        = 0x02
	adaptationBoth        = 0x03
)

// Packet is one parsed 188-byte transport packet. Data aliases the caller's
// buffer; payload extraction copies.
type Packet struct {
	Raw           []byte
	PID           uint16
	PUSI          bool
	TransportErr  bool
	CC            uint8
	HasPayload    bool
	HasAdapt      bool
	PCR           int64
	PCRSet        bool
	Discontinuity bool
	Payload       []byte
	Offset        int
}

// ParsePacket validates the sync byte and parses headers and the adaptation
// field. It rejects short buffers and scrambled packets.
func ParsePacket(raw []byte) (*Packet, error) {
	if len(raw) != PacketSize {
		return nil, clock.FatalError(clock.CodeTSInvalidSync, "ts.ParsePacket", "packet length %d != %d", len(raw), PacketSize)
	}
	if raw[0] != SyncByte {
		return nil, clock.FatalError(clock.CodeTSInvalidSync, "ts.ParsePacket", "bad sync byte %#x", raw[0])
	}
	if raw[1]&0x80 != 0 {
		return nil, clock.RecoverableError(clock.CodeTSInvalidSync, "ts.ParsePacket", "transport error indicator set")
	}
	p := &Packet{Raw: raw, PUSI: raw[1]&0x40 != 0, PID: uint16(raw[1]&0x1f)<<8 | uint16(raw[2])}
	p.TransportErr = raw[1]&0x80 != 0
	afc := (raw[3] >> 4) & 0x03
	p.CC = raw[3] & 0x0f
	p.HasAdapt = afc == adaptationOnly || afc == adaptationBoth
	p.HasPayload = afc == adaptationPayloadOnly || afc == adaptationBoth
	off := 4
	if p.HasAdapt {
		if off >= len(raw) {
			return nil, clock.FatalError(clock.CodeTSInvalidSync, "ts.ParsePacket", "missing adaptation length")
		}
		alen := int(raw[off])
		off++
		if alen > 0 {
			if off+alen > len(raw) {
				return nil, clock.FatalError(clock.CodeTSInvalidSync, "ts.ParsePacket", "adaptation overruns packet")
			}
			af := raw[off : off+alen]
			if af[0]&0x10 != 0 {
				if alen < 7 {
					return nil, clock.FatalError(clock.CodeTSInvalidSync, "ts.ParsePacket", "PCR flag but short adaptation")
				}
				base := uint64(af[1])<<25 | uint64(af[2])<<17 | uint64(af[3])<<9 | uint64(af[4])<<1 | uint64(af[5]>>7)
				ext := uint16(af[5]&0x01)<<8 | uint16(af[6])
				p.PCR = int64(base)*300 + int64(ext)
				p.PCRSet = true
			}
			if af[0]&0x80 != 0 && len(af) > 1 {
				p.Discontinuity = true
			}
		}
		off += alen
	}
	if p.HasPayload {
		if off > len(raw) {
			return nil, clock.FatalError(clock.CodeTSInvalidSync, "ts.ParsePacket", "payload offset %d beyond packet", off)
		}
		p.Offset = off
		p.Payload = raw[off:]
	}
	return p, nil
}

// SyncOffset locates the first 0x47 byte confirming at least three packets of
// alignment. It returns -1 when no trustworthy alignment exists.
func SyncOffset(buf []byte) int {
	for i := 0; i+PacketSize <= len(buf) && i < PacketSize; i++ {
		if buf[i] != SyncByte {
			continue
		}
		ok := true
		for j := i + PacketSize; j < len(buf); j += PacketSize {
			if buf[j] != SyncByte {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// SplitPackets scans a byte stream for 188-byte aligned packets. A trailing
// partial packet is returned as a truncated count rather than silently lost.
func SplitPackets(buf []byte) ([][]byte, int, error) {
	off := SyncOffset(buf)
	if off < 0 {
		return nil, 0, clock.FatalError(clock.CodeTSInvalidSync, "ts.SplitPackets", "no sync alignment")
	}
	body := buf[off:]
	n := len(body) / PacketSize
	pkts := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		pkt := body[i*PacketSize : (i+1)*PacketSize]
		if pkt[0] != SyncByte {
			return pkts, len(body) - n*PacketSize, clock.RecoverableError(clock.CodeTSInvalidSync, "ts.SplitPackets", "sync lost at packet %d", i)
		}
		pkts = append(pkts, pkt)
	}
	trunc := len(body) - n*PacketSize
	if trunc > 0 {
		return pkts, trunc, clock.RecoverableError(clock.CodeTruncated, "ts.SplitPackets", "%d trailing bytes", trunc)
	}
	return pkts, 0, nil
}

// Continuity tracks the 4-bit counter per PID and reports jumps. A duplicate
// counter on a packet without payload is legal and is not a jump.
type Continuity struct {
	last  map[uint16]uint8
	ready map[uint16]bool
	Jumps int
}

// NewContinuity builds a per-PID counter checker.
func NewContinuity() *Continuity {
	return &Continuity{last: make(map[uint16]uint8), ready: make(map[uint16]bool)}
}

// Check updates the counter and returns a discontinuity detail. A missing
// packet indicates a gap in the input rather than a broken stream.
func (c *Continuity) Check(p *Packet) error {
	if p.PID == PIDNull {
		return nil
	}
	if !c.ready[p.PID] {
		c.ready[p.PID] = true
		c.last[p.PID] = p.CC
		return nil
	}
	want := (c.last[p.PID] + 1) & 0x0f
	if p.CC == want {
		c.last[p.PID] = p.CC
		return nil
	}
	if p.CC == c.last[p.PID] && !p.HasPayload {
		return nil
	}
	c.Jumps++
	c.last[p.PID] = p.CC
	return clock.RecoverableError(clock.CodeTSContinuity, "ts.Continuity", "PID %#04x CC jumped from %d to %d", p.PID, want, p.CC)
}
