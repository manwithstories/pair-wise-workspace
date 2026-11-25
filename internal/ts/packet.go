// Package ts parses MPEG-TS structure: 188-byte packets, PSI tables, PCR/PTS
// timestamps, video access units and audio track descriptions. Every parse
// function is pure with respect to its inputs; stateful checks (continuity
// counters, section assembly) live in small tracker types so that tests can
// drive them packet by packet.
package ts

import (
	"fmt"

	"cinepipe/internal/fault"
)

// PacketSize is the ISO/IEC 13818-1 transport packet length in bytes.
const PacketSize = 188

// SyncByte opens every transport packet.
const SyncByte = 0x47

// NullPID carries stuffing packets the demultiplexer may discard.
const NullPID = 0x1FFF

// MaxAdaptationLen is the longest legal adaptation field payload (the whole
// packet minus header and the length byte itself).
const MaxAdaptationLen = PacketSize - 5

// AdaptationField holds the parsed adaptation field flags. Raw retains the
// bytes so the PCR parser can read the clock reference without this type
// depending on pcr.go.
type AdaptationField struct {
	Length        uint8
	Discontinuity bool
	RandomAccess  bool
	ESPriority    bool
	HasPCR        bool
	HasOPCR       bool
	HasSeamless   bool
	HasExtension  bool
	Raw           []byte
}

// Packet is one transport packet with its location in the input.
type Packet struct {
	Offset int64 // byte offset of the sync byte within the input
	Index  int64 // zero-based packet ordinal

	TEI         bool
	PUSI        bool
	TransportEP bool
	Scrambled   bool
	PID         uint16
	CC          uint8

	HasAdaptation bool
	HasPayload    bool
	Adaptation    *AdaptationField
	Payload       []byte

	// Short is true when the final chunk of input was under 188 bytes. A short
	// packet is reported as truncated but still exposes the bytes it has so the
	// scanner can advance to the next stream.
	Short bool
}

// CarriesPCR reports whether an adaptation field with a PCR is present.
func (p *Packet) CarriesPCR() bool { return p.Adaptation != nil && p.Adaptation.HasPCR }

// Errorf builds a located diagnostic anchored at this packet.
func (p *Packet) Errorf(kind fault.Kind, detail string) fault.Error {
	return fault.Error{Kind: kind, Offset: p.Offset, PID: p.PID, Packet: p.Index, Detail: detail}
}

// ParsePacket decodes a chunk of at most 188 bytes. A chunk shorter than the
// transport size is marked Short and reported as truncated; its bytes are still
// parsed as far as the header and adaptation field allow. Returned diagnostics
// are already located at off.
func ParsePacket(raw []byte, off, index int64) (*Packet, []fault.Error) {
	p := &Packet{Offset: off, Index: index, Short: len(raw) < PacketSize}
	var errs []fault.Error
	add := func(kind fault.Kind, detail string) {
		errs = append(errs, fault.Error{Kind: kind, Offset: off, Packet: index, Detail: detail})
	}

	if len(raw) == 0 {
		add(fault.KindTruncated, "empty chunk")
		return p, errs
	}
	if raw[0] != SyncByte {
		add(fault.KindSync, fmt.Sprintf("sync byte 0x%02x", raw[0]))
		return p, errs
	}
	if len(raw) < 4 {
		add(fault.KindTruncated, fmt.Sprintf("header needs 4 bytes, have %d", len(raw)))
		// Fill in what is available without reading past the chunk.
		p.TEI = raw[1]&0x80 != 0
		p.PUSI = raw[1]&0x40 != 0
		p.TransportEP = raw[1]&0x20 != 0
		p.PID = uint16(raw[1]&0x1F)<<8 | uint16(raw[2])
		return p, errs
	}
	if len(raw) < PacketSize {
		add(fault.KindTruncated, fmt.Sprintf("packet has %d of %d bytes", len(raw), PacketSize))
	}

	p.TEI = raw[1]&0x80 != 0
	p.PUSI = raw[1]&0x40 != 0
	p.TransportEP = raw[1]&0x20 != 0
	p.PID = uint16(raw[1]&0x1F)<<8 | uint16(raw[2])
	p.Scrambled = raw[3]&0xC0 != 0
	p.CC = raw[3] & 0x0F
	afc := (raw[3] >> 4) & 0x03

	if afc == 0 {
		add(fault.KindAdaptation, "reserved adaptation_field_control 0")
		return p, errs
	}
	pos := 4
	if afc&0x02 != 0 {
		p.HasAdaptation = true
		if pos >= len(raw) {
			add(fault.KindAdaptation, "adaptation field missing length byte")
			return p, errs
		}
		length := int(raw[pos])
		p.Adaptation = &AdaptationField{Length: uint8(length)}
		pos++
		if length > MaxAdaptationLen {
			add(fault.KindAdaptation, fmt.Sprintf("adaptation length %d exceeds %d", length, MaxAdaptationLen))
			// Clamp so we can still locate the payload boundary.
			length = MaxAdaptationLen
		}
		end := pos + length
		if end > len(raw) {
			add(fault.KindAdaptation, fmt.Sprintf("adaptation overruns packet by %d bytes", end-len(raw)))
			end = len(raw)
		}
		if length > 0 && pos < len(raw) {
			flags := raw[pos]
			p.Adaptation.Discontinuity = flags&0x80 != 0
			p.Adaptation.RandomAccess = flags&0x40 != 0
			p.Adaptation.ESPriority = flags&0x20 != 0
			p.Adaptation.HasPCR = flags&0x10 != 0
			p.Adaptation.HasOPCR = flags&0x08 != 0
			p.Adaptation.HasSeamless = flags&0x04 != 0
			p.Adaptation.HasExtension = flags&0x01 != 0
			p.Adaptation.Raw = raw[pos+1 : end]
		}
		pos = end
	}
	if afc&0x01 != 0 {
		p.HasPayload = true
		if pos < len(raw) {
			p.Payload = raw[pos:]
		}
	}
	if pos > len(raw) {
		add(fault.KindAdaptation, "payload begins past available bytes")
	}
	return p, errs
}

// ContinuityTracker validates continuity_counter progression per PID. A PID
// whose counter jumps by more than one is the signature of dropped packets;
// the tracker reports it once per jump and records the affected PID so the
// scanner can close a damaged range. Packets that carry only an adaptation
// field must repeat their counter; a changed counter there is reported too.
type ContinuityTracker struct {
	last    map[uint16]uint8
	seen    map[uint16]bool
	skip    map[uint16]bool // discontinuity_indicator armed for the next packet
	packets map[uint16]int64
}

// NewContinuityTracker returns an empty tracker.
func NewContinuityTracker() *ContinuityTracker {
	return &ContinuityTracker{
		last:    make(map[uint16]uint8),
		seen:    make(map[uint16]bool),
		skip:    make(map[uint16]bool),
		packets: make(map[uint16]int64),
	}
}

// Observe records a packet and returns any continuity diagnostics. A packet
// with the adaptation discontinuity_indicator set arms a one-shot skip so the
// next counter value is accepted without complaint.
func (t *ContinuityTracker) Observe(p *Packet) []fault.Error {
	if p.PID == NullPID || p.Scrambled {
		return nil
	}
	if p.Adaptation != nil && p.Adaptation.Discontinuity {
		t.seen[p.PID] = true
		t.last[p.PID] = p.CC
		t.skip[p.PID] = true
		return nil
	}
	if !t.seen[p.PID] {
		t.seen[p.PID] = true
		t.last[p.PID] = p.CC
		return nil
	}
	prev := t.last[p.PID]
	if t.skip[p.PID] {
		t.skip[p.PID] = false
		t.last[p.PID] = p.CC
		return nil
	}
	t.packets[p.PID]++
	if p.HasPayload {
		want := (prev + 1) & 0x0F
		if p.CC != want {
			gap := int(p.CC-want) & 0x0F
			t.last[p.PID] = p.CC
			if gap == 0x0F {
				return []fault.Error{p.Errorf(fault.KindContinuity,
					fmt.Sprintf("duplicate packet cc=%d", p.CC))}
			}
			return []fault.Error{p.Errorf(fault.KindContinuity,
				fmt.Sprintf("expected cc=%d got %d, lost %d packet(s)", want, p.CC, gap))}
		}
		t.last[p.PID] = p.CC
		return nil
	}
	// Adaptation-only packet: the counter is not supposed to advance.
	if p.CC != prev {
		t.last[p.PID] = p.CC
		return []fault.Error{p.Errorf(fault.KindContinuity,
			fmt.Sprintf("adaptation-only packet advanced cc %d->%d", prev, p.CC))}
	}
	return nil
}

// Packets returns how many payload-bearing packets have been observed on a PID.
func (t *ContinuityTracker) Packets(pid uint16) int64 { return t.packets[pid] }

// LastCC returns the last counter seen for a PID.
func (t *ContinuityTracker) LastCC(pid uint16) (uint8, bool) {
	cc, ok := t.last[pid]
	return cc, ok
}
