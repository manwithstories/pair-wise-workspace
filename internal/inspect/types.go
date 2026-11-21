// Package inspect consumes ordered parse events and produces the stream
// structure report: programs, tracks, timestamps, keyframe index and
// segment boundaries. It shares the clock and fault model with every
// other subsystem and drives a Sink (the repack stage) with bounded
// memory: packets flow through, they are never accumulated.
package inspect

import (
	"tsbatch/internal/fault"
	"tsbatch/internal/ts"
)

// PacketEvent is one parsed packet as produced by a worker. Raw carries
// the verbatim packet so downstream stages can repackage it.
type PacketEvent struct {
	Index  uint64
	Header ts.Header
	Raw    *[ts.PacketSize]byte
	HasPTS bool
	PTSRaw uint64 // 33-bit, not yet unwrapped
	// Media marks packets whose payload belongs to an elementary stream
	// (set by Analyzer before Sink.Append; the repack stage hashes only
	// media payloads).
	Media bool
}

// ParsePacket converts a raw packet into a PacketEvent (worker stage).
func ParsePacket(idx uint64, raw *[ts.PacketSize]byte) (PacketEvent, error) {
	h, err := ts.ParseHeader(raw)
	if err != nil {
		return PacketEvent{}, err
	}
	ev := PacketEvent{Index: idx, Header: h, Raw: raw}
	if h.PUSI && h.PayloadOff >= 0 {
		if pts, ok := pesPTS(raw[h.PayloadOff:]); ok {
			ev.HasPTS, ev.PTSRaw = true, pts
		}
	}
	return ev, nil
}

// pesPTS extracts the 33-bit PTS from a PES header, if present.
func pesPTS(p []byte) (uint64, bool) {
	if len(p) < 14 || p[0] != 0 || p[1] != 0 || p[2] != 1 {
		return 0, false
	}
	if p[7]&0x80 == 0 {
		return 0, false
	}
	b := p[9:]
	return uint64(b[0]>>1&0x07)<<30 | uint64(b[1])<<22 |
		uint64(b[2]>>1&0x7F)<<15 | uint64(b[3])<<7 | uint64(b[4]>>1)&0x7F, true
}

// Keyframe is one random-access point on the unified timeline.
type Keyframe struct {
	PacketIndex uint64
	Time        int64 // 90 kHz
}

// Program describes one PMT program.
type Program struct {
	Number  uint16
	PMTPID  uint16
	PCRPID  uint16
	Streams []ts.PMTEntry
}

// Isolation records a damaged zone kept apart from the valid segments.
type Isolation struct {
	StartPacket, EndPacket uint64
	Reason                 fault.Category
}

// Sink receives the ordered repackaging stream. Implementations must not
// retain Raw pointers beyond the call.
type Sink interface {
	BeginSegment(id int, firstPacket uint64, time int64)
	Append(ev PacketEvent)
	EndSegment(id int, lastPacket uint64, time int64)
	Isolate(start, end uint64, reason fault.Category)
}

// Report is the deterministic analysis result.
type Report struct {
	Programs      []Program
	Keyframes     []Keyframe
	Faults        fault.List
	PCRWraps      int
	PSIUpdates    int
	Segments      int
	Isolations    []Isolation
	MaxKFInterval int64 // 90 kHz
}
