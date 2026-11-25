package ts

import (
	"encoding/binary"
	"streamcut/internal/media"
)

// PES packet start-code prefix 0x000001 and stream-id layout.
const (
	pesPrefix = 0x000001
	sidAudio  = 0xC0 // PCM private audio stream id
	sidVideo  = 0xE0
)

// PES is one reassembled PES packet with parsed PTS/DTS (still raw 33-bit;
// the pipeline extends them with per-PID unwrappers).
type PES struct {
	PID    uint16
	StreamID byte
	PTS    uint64
	DTS    uint64
	HasPTS bool
	HasDTS bool
	Data   []byte // PES packet data (frame bytes / raw audio), copied
}

// PESAssembler collects TS payloads per PID until a PUSI closes the unit.
// Buffers are hard-capped; an overlong unit is an unrecoverable error rather
// than an unbounded allocation.
type PESAssembler struct {
	maxUnit int
	buf     map[uint16][]byte
	open    map[uint16]bool
}

// NewPESAssembler sets the per-PID reassembly cap in bytes.
func NewPESAssembler(maxUnit int) *PESAssembler {
	return &PESAssembler{
		maxUnit: maxUnit,
		buf:     map[uint16][]byte{},
		open:    map[uint16]bool{},
	}
}

// Push feeds one packet's payload. When p.PUSI closes a previously open unit,
// that unit is parsed and returned; otherwise ok is false and more payload is
// expected. A PUSI with no prior unit simply starts one.
func (a *PESAssembler) Push(p *Packet) (pes *PES, ok bool, err error) {
	if !p.HasPayload || p.PID == PIDPAT || p.PID == PIDNull {
		return nil, false, nil
	}
	if p.PUSI {
		if old, exists := a.buf[p.PID]; exists && a.open[p.PID] {
			parsed, perr := parsePES(p.PID, old)
			a.buf[p.PID] = a.buf[p.PID][:0]
			if perr != nil {
				return nil, false, perr
			}
			pes, ok = parsed, true
		}
		a.open[p.PID] = true
	}
	if !a.open[p.PID] {
		// Payload without PUSI and no open unit: drop silently (mid-stream
		// join) rather than allocating on garbage.
		return pes, ok, nil
	}
	if len(a.buf[p.PID])+len(p.Payload) > a.maxUnit {
		return nil, false, media.Wrap(media.EResourceLimit, "ts.pes",
			"PES reassembly exceeded hard cap", nil)
	}
	a.buf[p.PID] = append(a.buf[p.PID], p.Payload...)
	return pes, ok, nil
}

// Flush returns any still-open unit at end of input (nil if none).
func (a *PESAssembler) Flush(pid uint16) (*PES, error) {
	old, exists := a.buf[pid]
	if !exists || !a.open[pid] || len(old) == 0 {
		return nil, nil
	}
	a.buf[pid] = old[:0]
	a.open[pid] = false
	return parsePES(pid, old)
}

func parsePES(pid uint16, b []byte) (*PES, error) {
	if len(b) < 9 || uint32(b[0])<<16|uint32(b[1])<<8|uint32(b[2]) != pesPrefix {
		return nil, media.Wrap(media.EUnrecoverable, "ts.pes",
			"bad PES start code", nil)
	}
	out := &PES{PID: pid, StreamID: b[3]}
	hdrLen := int(b[8])
	dataOff := 9 + hdrLen
	if len(b) < dataOff {
		return nil, media.Wrap(media.ETruncated, "ts.pes",
			"PES header length exceeds packet", nil)
	}
	ptsDtsFlags := b[7] >> 6
	if ptsDtsFlags >= 2 {
		pts, ok1 := readTimestamp(b[9:])
		if !ok1 {
			return nil, media.Wrap(media.EUnrecoverable, "ts.pes",
				"malformed PTS", nil)
		}
		out.PTS, out.HasPTS = pts, true
		if ptsDtsFlags == 3 && hdrLen >= 10 {
			dts, ok2 := readTimestamp(b[14:])
			if !ok2 {
				return nil, media.Wrap(media.EUnrecoverable, "ts.pes",
					"malformed DTS", nil)
			}
			out.DTS, out.HasDTS = dts, true
		}
	}
	out.Data = append([]byte(nil), b[dataOff:]...)
	return out, nil
}

// readTimestamp decodes the 33-bit '1110...' / '0001...' PTS/DTS layout.
func readTimestamp(b []byte) (uint64, bool) {
	if len(b) < 5 {
		return 0, false
	}
	if b[0]&0x01 == 0 || b[2]&0x01 == 0 || b[4]&0x01 == 0 {
		return 0, false
	}
	ts := uint64(b[0]&0x0E)<<29 | uint64(b[1])<<22 |
		uint64(b[2]&0xFE)<<14 | uint64(b[3])<<7 |
		uint64(b[4])>>1
	return ts, true
}

// EncodeTimestamp writes a 33-bit PTS in the 5-byte marker layout, using the
// 4-bit tag (0x2 for PTS-only, 0x3 for PTS of a PTS/DTS pair, 0x1 for DTS).
func EncodeTimestamp(ts uint64, tag byte) [5]byte {
	var b [5]byte
	b[0] = (tag << 4) | byte((ts>>30)&0x07)<<1 | 1
	b[1] = byte((ts >> 22) & 0xFF)
	b[2] = byte((ts>>14)&0xFE) | 1
	b[3] = byte((ts >> 7) & 0xFF)
	b[4] = byte((ts&0x7F)<<1) | 1
	return b
}

// BuildPES constructs a PES packet byte stream (header + payload) for one
// elementary stream, used by the fixture generator.
func BuildPES(streamID byte, pts, dts uint64, hasDTS bool, payload []byte) []byte {
	hdr := make([]byte, 0, 24)
	hdr = append(hdr, 0x00, 0x00, 0x01, streamID)
	lenPos := len(hdr)
	hdr = append(hdr, 0, 0) // PES packet length, patched below
	hdr = append(hdr, 0x80) // marker bits '10', no scrambling
	flags := byte(0x80)
	if hasDTS {
		flags = 0xC0
	}
	hdr = append(hdr, flags)
	tsStart := len(hdr)
	if hasDTS {
		hdr = append(hdr, 10)
		p := EncodeTimestamp(pts, 0x3)
		hdr = append(hdr, p[:]...)
		d := EncodeTimestamp(dts, 0x1)
		hdr = append(hdr, d[:]...)
	} else {
		hdr = append(hdr, 5)
		p := EncodeTimestamp(pts, 0x2)
		hdr = append(hdr, p[:]...)
	}
	out := append(hdr, payload...)
	pesLen := len(out) - (tsStart - 2)
	binary.BigEndian.PutUint16(out[lenPos:], uint16(pesLen))
	return out
}
