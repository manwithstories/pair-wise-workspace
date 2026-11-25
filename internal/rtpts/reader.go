// Package rtpts unwraps RTP-carried MPEG-TS. A capture file is a length-
// prefixed record stream so that fixtures are reproducible byte for byte:
//
//	magic "RTPC1" (5 bytes)
//	repeated: uint32 big-endian length, then that many bytes of RTP packet
//
// Each RTP packet is decoded with pion/rtp. The depacketiser reorders
// out-of-sequence packets back to their transmitted order, drops duplicates,
// reports losses, and concatenates payloads into the TS byte stream. That
// stream is then handed to the same internal/ts parser as a bare transport
// stream, so both sources share one parse path.
package rtpts

import (
	"encoding/binary"
	"fmt"

	"github.com/pion/rtp"

	"cinepipe/internal/fault"
)

// Magic opens a cinepipe RTP capture.
const Magic = "RTPC1"

// ErrMagic is returned when the capture does not begin with the magic.
var ErrMagic = fmt.Errorf("rtpts: missing %q magic", Magic)

// Record is one captured RTP packet with its location in the capture file.
type Record struct {
	CaptureOffset int64
	Arrival       int // zero-based arrival order
	Packet        rtp.Packet
}

// Gap is a contiguous run of missing sequence numbers.
type Gap struct {
	Before uint16
	After  uint16
	Count  int
}

// Reorder records a packet that arrived after a higher sequence number.
type Reorder struct {
	Seq      uint16
	ArrivedAfter uint16
}

// Stats summarises transport-level outcomes for the scan report.
type Stats struct {
	Packets    int
	Duplicates int
	Reordered  int
	Lost       int
	// TSBytes is the length of the reconstructed transport stream.
	TSBytes int
}

// Stream is the reconstructed transport stream plus its transport diagnostics.
type Stream struct {
	Bytes  []byte
	Stats  Stats
	Gaps   []Gap
	Reorders []Reorder
	Errors []fault.Error
}

// ReadCapture decodes the length-prefixed capture framing. A record whose
// declared length runs past the file is reported as truncated and stops the
// read; a malformed RTP packet is reported and skipped so the rest still loads.
func ReadCapture(data []byte) ([]Record, []fault.Error) {
	var recs []Record
	var errs []fault.Error
	if len(data) < len(Magic) || string(data[:len(Magic)]) != Magic {
		return nil, []fault.Error{{Kind: fault.KindRTP, Offset: 0, Detail: "capture magic missing"}}
	}
	pos := int64(len(Magic))
	for pos < int64(len(data)) {
		if int64(len(data))-pos < 4 {
			errs = append(errs, fault.Error{Kind: fault.KindRTP, Offset: pos,
				Detail: "capture record length truncated"})
			break
		}
		n := int64(binary.BigEndian.Uint32(data[pos:]))
		body := pos + 4
		if n <= 0 {
			errs = append(errs, fault.Error{Kind: fault.KindRTP, Offset: pos,
				Detail: "capture record has zero length"})
			pos = body
			continue
		}
		if body+n > int64(len(data)) {
			errs = append(errs, fault.Error{Kind: fault.KindRTP, Offset: body,
				Detail: fmt.Sprintf("capture record claims %d bytes, %d remain", n, int64(len(data))-body)})
			break
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(data[body : body+n]); err != nil {
			errs = append(errs, fault.Error{Kind: fault.KindRTP, Offset: body,
				Detail: fmt.Sprintf("rtp unmarshal: %v", err)})
			pos = body + n
			continue
		}
		recs = append(recs, Record{CaptureOffset: body, Arrival: len(recs), Packet: pkt})
		pos = body + n
	}
	return recs, errs
}

// depacketised is one accepted packet after sequence analysis.
type depacketised struct {
	seq      uint16
	unwrapped int64
	arrival  int
	record   Record
}

// Depacketize orders the captured packets by their unwrapped sequence number,
// drops duplicates, and concatenates payloads. Reordering is restored silently
// while being reported; losses are reported and left as holes, which the TS
// layer then sees as continuity_counter jumps.
func Depacketize(recs []Record) *Stream {
	s := &Stream{}
	if len(recs) == 0 {
		return s
	}
	// Establish the sequence baseline from the first arrival.
	base := int64(recs[0].Packet.SequenceNumber)
	seen := make(map[uint16]bool, len(recs))
	var accepted []depacketised
	highest := int64(-1)
	for _, r := range recs {
		raw := r.Packet.SequenceNumber
		if seen[raw] {
			s.Stats.Duplicates++
			s.Errors = append(s.Errors, fault.Error{Kind: fault.KindRTP,
				Offset: r.CaptureOffset, Detail: fmt.Sprintf("duplicate rtp seq=%d", raw)})
			continue
		}
		seen[raw] = true
		seq := unwrapSeq(base, raw)
		if highest >= 0 && seq < highest {
			s.Stats.Reordered++
			s.Reorders = append(s.Reorders, Reorder{Seq: raw, ArrivedAfter: uint16(highest + base)})
			s.Errors = append(s.Errors, fault.Error{Kind: fault.KindRTP,
				Offset: r.CaptureOffset, Detail: fmt.Sprintf("rtp reorder seq=%d after %d", raw, highest+base)})
		}
		if seq > highest {
			highest = seq
		}
		accepted = append(accepted, depacketised{seq: raw, unwrapped: seq, arrival: r.Arrival, record: r})
	}
	// Sort by unwrapped sequence number to rebuild the transmitted order.
	sortBySeq(accepted)

	// Count losses between consecutive accepted sequence numbers.
	var prev *depacketised
	for i := range accepted {
		cur := &accepted[i]
		if prev != nil {
			if delta := cur.unwrapped - prev.unwrapped; delta > 1 {
				g := Gap{Before: prev.seq, After: cur.seq, Count: int(delta - 1)}
				if g.Count > 0 {
					s.Gaps = append(s.Gaps, g)
					s.Stats.Lost += g.Count
					s.Errors = append(s.Errors, fault.Error{Kind: fault.KindRTP,
						Offset: cur.record.CaptureOffset,
						Detail: fmt.Sprintf("rtp loss %d packet(s) between seq=%d and %d", g.Count, prev.seq, cur.seq)})
				}
			}
		}
		prev = cur
	}

	// Concatenate payloads in transmitted order.
	total := 0
	for _, d := range accepted {
		total += len(d.record.Packet.Payload)
	}
	s.Bytes = make([]byte, 0, total)
	for _, d := range accepted {
		if len(d.record.Packet.Payload) == 0 {
			s.Errors = append(s.Errors, fault.Error{Kind: fault.KindRTP,
				Offset: d.record.CaptureOffset, Detail: fmt.Sprintf("rtp seq=%d has empty payload", d.seq)})
			continue
		}
		s.Bytes = append(s.Bytes, d.record.Packet.Payload...)
	}
	s.Stats.Packets = len(accepted)
	s.Stats.TSBytes = len(s.Bytes)
	return s
}

// unwrapSeq expresses seq relative to base across the 16-bit wrap.
func unwrapSeq(base int64, seq uint16) int64 {
	d := int64(seq) - (base & 0xFFFF)
	if d < -0x8000 {
		d += 0x10000
	} else if d > 0x8000 {
		d -= 0x10000
	}
	return base + d
}

// sortBySeq sorts accepted packets by unwrapped sequence number, preserving
// arrival order for ties (there are none, since duplicates are dropped).
func sortBySeq(p []depacketised) {
	// Insertion sort: captures in these fixtures are small and mostly ordered.
	for i := 1; i < len(p); i++ {
		for j := i; j > 0 && p[j].unwrapped < p[j-1].unwrapped; j-- {
			p[j], p[j-1] = p[j-1], p[j]
		}
	}
}

// Parse reads a capture and returns the reconstructed transport stream.
func Parse(data []byte) (*Stream, error) {
	recs, errs := ReadCapture(data)
	if len(recs) == 0 && len(errs) > 0 && string(data[:min(len(data), len(Magic))]) != Magic {
		return nil, ErrMagic
	}
	s := Depacketize(recs)
	s.Errors = append(s.Errors, errs...)
	return s, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
