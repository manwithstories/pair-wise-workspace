// Package ts scans MPEG-TS inputs at 188-byte packet boundaries, parses
// packets with astits and reports classified faults with byte offsets.
package ts

import (
	"bytes"
	"context"
	"fmt"

	"github.com/asticode/go-astits"

	"tscheck/internal/model"
)

const (
	// PacketSize is the MPEG-TS packet size in bytes.
	PacketSize = 188
	// SyncByte marks the start of every packet.
	SyncByte = 0x47
)

// ParsePacket parses a single 188-byte packet using the astits demuxer.
func ParsePacket(b []byte) (*astits.Packet, error) {
	if len(b) != PacketSize {
		return nil, fmt.Errorf("ts: packet must be %d bytes, got %d", PacketSize, len(b))
	}
	if b[0] != SyncByte {
		return nil, fmt.Errorf("ts: missing sync byte")
	}
	dmx := astits.NewDemuxer(context.Background(), bytes.NewReader(b),
		astits.DemuxerOptPacketSize(PacketSize))
	p, err := dmx.NextPacket()
	if err != nil {
		return nil, fmt.Errorf("ts: parse packet: %w", err)
	}
	return p, nil
}

// PacketInfo is the parsed view of one packet handed to downstream layers.
type PacketInfo struct {
	Index   int
	Offset  int64
	PID     uint16
	CC      uint8
	PUSI    bool
	Payload []byte // references the input buffer
	HasPCR  bool
	PCRBase int64 // 33-bit, 90 kHz
	PCRExt  int64 // 9-bit, 27 MHz
}

// Report summarizes one scan pass.
type Report struct {
	Packets          int
	Duplicates       int
	FirstFaultOffset int64 // -1 when the input is clean
	Faults           []model.Fault
}

// Scanner walks packet boundaries and validates sync and continuity
// counters. State is per-PID; a Scanner must not be reused across inputs.
type Scanner struct {
	lastCC map[uint16]uint8
	seen   map[uint16]bool
}

// NewScanner creates a Scanner.
func NewScanner() *Scanner {
	return &Scanner{lastCC: map[uint16]uint8{}, seen: map[uint16]bool{}}
}

// Scan walks data at 188-byte boundaries. It reports the first anomalous
// offset, records each continuity gap exactly once, counts duplicated
// packets without raising errors for them, and resynchronizes after sync
// loss. Packets after a fault remain linked to it through their offsets.
func (s *Scanner) Scan(data []byte) ([]PacketInfo, *Report) {
	rep := &Report{FirstFaultOffset: -1}
	var out []PacketInfo
	addFault := func(f model.Fault) {
		if rep.FirstFaultOffset < 0 {
			rep.FirstFaultOffset = f.Offset
		}
		rep.Faults = append(rep.Faults, f)
	}
	off := 0
	idx := 0
	for off < len(data) {
		if len(data)-off < PacketSize {
			addFault(model.Fault{Kind: model.FaultTruncated, Offset: int64(off), PID: -1,
				Detail: fmt.Sprintf("%d trailing bytes", len(data)-off)})
			break
		}
		if data[off] != SyncByte {
			addFault(model.Fault{Kind: model.FaultSyncLoss, Offset: int64(off), PID: -1,
				Detail: "sync byte mismatch"})
			next := resync(data, off+1)
			if next < 0 {
				break
			}
			off = next
			continue
		}
		pkt, err := ParsePacket(data[off : off+PacketSize])
		if err != nil {
			addFault(model.Fault{Kind: model.FaultSyncLoss, Offset: int64(off), PID: -1,
				Detail: err.Error()})
			off += PacketSize
			continue
		}
		h := pkt.Header
		pi := PacketInfo{Index: idx, Offset: int64(off), PID: h.PID,
			CC: h.ContinuityCounter, PUSI: h.PayloadUnitStartIndicator,
			Payload: pkt.Payload}
		if pkt.AdaptationField != nil && pkt.AdaptationField.PCR != nil {
			pi.HasPCR = true
			pi.PCRBase = pkt.AdaptationField.PCR.Base
			pi.PCRExt = pkt.AdaptationField.PCR.Extension
		}
		if h.HasPayload {
			if s.seen[h.PID] {
				exp := (s.lastCC[h.PID] + 1) & 0x0F
				switch h.ContinuityCounter {
				case exp:
				case s.lastCC[h.PID]:
					rep.Duplicates++ // repeated packet: counted, never an error
				default:
					addFault(model.Fault{Kind: model.FaultContinuityGap,
						Offset: int64(off), PID: int(h.PID),
						Detail: fmt.Sprintf("cc %d -> %d, want %d",
							s.lastCC[h.PID], h.ContinuityCounter, exp)})
				}
			}
			s.lastCC[h.PID] = h.ContinuityCounter
			s.seen[h.PID] = true
		}
		out = append(out, pi)
		rep.Packets++
		off += PacketSize
		idx++
	}
	return out, rep
}

// resync finds the next plausible packet boundary: a sync byte followed by
// another sync byte exactly one packet later.
func resync(data []byte, from int) int {
	for i := from; i < len(data); i++ {
		if data[i] != SyncByte {
			continue
		}
		if i+PacketSize >= len(data) || data[i+PacketSize] == SyncByte {
			return i
		}
	}
	return -1
}
