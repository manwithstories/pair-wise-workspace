// Package ts parses MPEG-2 Transport Streams: fixed 188-byte packets,
// adaptation fields (including PCR), continuity-counter tracking and PES
// reassembly. PSI (PAT/PMT) parsing lives in psi.go.
package ts

import (
	"streamcut/internal/media"
)

// PacketSize is the fixed MPEG-TS packet length in bytes.
const PacketSize = 188

// Well-known PIDs used by the generated streams and accepted on input.
const (
	PIDPAT uint16 = 0x0000
	PIDNull uint16 = 0x1FFF
)

// Packet is the parsed view of one 188-byte TS packet. Payload aliases the
// input buffer and stays valid only until the next packet is parsed.
type Packet struct {
	PUSI         bool
	PID          uint16
	Priority     bool
	HasAdapt     bool
	HasPayload   bool
	CC           uint8
	Adaptation   []byte // raw adaptation field bytes (excluding length byte)
	Payload      []byte
	HasPCR       bool
	PCR          uint64 // full 42-bit 27 MHz PCR when HasPCR
	RandomAccess bool   // adaptation field random-access indicator
}

// Parse decodes one 188-byte packet. A short buffer or a bad sync byte is an
// unrecoverable input error.
func Parse(buf []byte) (*Packet, error) {
	if len(buf) < PacketSize {
		return nil, media.Wrap(media.ETruncated, "ts.parse",
			"packet shorter than 188 bytes", nil)
	}
	if buf[0] != 0x47 {
		return nil, media.Wrap(media.EUnrecoverable, "ts.parse",
			"missing 0x47 sync byte", nil)
	}
	p := &Packet{
		PUSI:       buf[1]&0x40 != 0,
		PID:        uint16(buf[1]&0x1F)<<8 | uint16(buf[2]),
		Priority:   buf[1]&0x20 != 0,
		HasAdapt:   buf[3]&0x20 != 0,
		HasPayload: buf[3]&0x10 != 0,
		CC:         buf[3] & 0x0F,
	}
	off := 4
	if p.HasAdapt {
		alen := int(buf[off])
		off++
		if alen > PacketSize-off {
			return nil, media.Wrap(media.EUnrecoverable, "ts.parse",
				"adaptation field exceeds packet", nil)
		}
		p.Adaptation = buf[off : off+alen]
		if err := p.parseAdaptation(); err != nil {
			return nil, err
		}
		off += alen
	}
	if p.HasPayload {
		if off > PacketSize {
			return nil, media.Wrap(media.EUnrecoverable, "ts.parse",
				"payload offset past packet end", nil)
		}
		p.Payload = buf[off:PacketSize]
	}
	return p, nil
}

func (p *Packet) parseAdaptation() error {
	a := p.Adaptation
	if len(a) == 0 {
		return nil // stuffing-only adaptation field is legal
	}
	flags := a[0]
	p.RandomAccess = flags&0x40 != 0
	off := 1
	if flags&0x10 != 0 { // PCR flag
		if len(a) < off+6 {
			return media.Wrap(media.EUnrecoverable, "ts.adapt",
				"PCR flag set but field truncated", nil)
		}
		base := uint64(a[off])<<25 | uint64(a[off+1])<<17 |
			uint64(a[off+2])<<9 | uint64(a[off+3])<<1 |
			uint64(a[off+4])>>7
		ext := uint64(a[off+4]&0x01)<<8 | uint64(a[off+5])
		p.PCR = base*300 + ext
		p.HasPCR = true
	}
	return nil
}

// CCTracker watches per-PID continuity counters. It reports duplicate packets
// and discontinuities; a PID seen for the first time only anchors state.
type CCTracker struct {
	last   map[uint16]uint8
	known  map[uint16]bool
	Errors int // number of discontinuities (cc_error in the metrics)
	Dupes  int
}

// NewCCTracker returns an empty tracker.
func NewCCTracker() *CCTracker {
	return &CCTracker{last: map[uint16]uint8{}, known: map[uint16]bool{}}
}

// Observe feeds one packet. It returns a media.ErrCCGap-classified error on a
// discontinuity; the caller logs it and keeps processing. Packets carrying no
// payload byte (adaptation-only, CC may legitimately repeat) are anchored.
func (t *CCTracker) Observe(p *Packet) error {
	if p.PID == PIDNull {
		return nil
	}
	if !t.known[p.PID] {
		t.known[p.PID] = true
		t.last[p.PID] = p.CC
		return nil
	}
	prev := t.last[p.PID]
	if p.CC == prev {
		if p.HasPayload {
			t.Dupes++
		}
		return nil
	}
	if want := (prev + 1) & 0x0F; p.CC != want {
		t.Errors++
		t.last[p.PID] = p.CC
		return media.Wrap(media.ECCGap, "ts.cc",
			"continuity counter jump", nil)
	}
	t.last[p.PID] = p.CC
	return nil
}
