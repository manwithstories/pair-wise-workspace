// Package ts implements MPEG-TS packet validation, streaming packet
// framing over arbitrary short reads, continuity-counter tracking and
// PSI section reassembly. Only 188-byte packets are supported.
package ts

import (
	"errors"
	"fmt"
	"io"
)

// PacketSize is the only supported MPEG-TS packet length.
const PacketSize = 188

// SyncByte starts every valid packet.
const SyncByte = 0x47

// ErrBadPacketSize rejects unsupported configured packet lengths.
var ErrBadPacketSize = errors.New("ts: unsupported packet size (only 188)")

// ErrSync reports a packet that does not start with the sync byte.
var ErrSync = errors.New("ts: sync byte 0x47 not found")

// Header carries the parsed fields of one 188-byte packet.
type Header struct {
	TEI          bool   // transport error indicator
	PUSI         bool   // payload unit start indicator
	PID          uint16 // 13-bit packet identifier
	HasAdapt     bool   // adaptation field present
	HasPayload   bool   // payload present
	CC           uint8  // 4-bit continuity counter
	HasPCR       bool   // adaptation field carries a PCR
	PCR          uint64 // 33-bit PCR base, 90 kHz units
	RandomAccess bool   // adaptation random_access_indicator (keyframe)
	PayloadOff   int    // offset of payload inside the packet, -1 if none
}

// ParseHeader validates and decodes the header of one packet.
func ParseHeader(pkt *[PacketSize]byte) (Header, error) {
	var h Header
	if pkt[0] != SyncByte {
		return h, ErrSync
	}
	h.TEI = pkt[1]&0x80 != 0
	h.PUSI = pkt[1]&0x40 != 0
	h.PID = uint16(pkt[1]&0x1F)<<8 | uint16(pkt[2])
	afc := pkt[3] >> 4 & 0x3
	h.HasAdapt = afc == 2 || afc == 3
	h.HasPayload = afc == 1 || afc == 3
	h.CC = pkt[3] & 0x0F
	off := 4
	if h.HasAdapt {
		afl := int(pkt[4])
		if 5+afl > PacketSize {
			return h, fmt.Errorf("ts: adaptation field overruns packet")
		}
		if afl >= 7 && pkt[5]&0x10 != 0 { // PCR_flag
			b := pkt[6:]
			h.HasPCR = true
			h.PCR = uint64(b[0])<<25 | uint64(b[1])<<17 | uint64(b[2])<<9 |
				uint64(b[3])<<1 | uint64(b[4])>>7
		}
		h.RandomAccess = afl >= 1 && pkt[5]&0x40 != 0 // random_access_indicator
		off = 5 + afl
	}
	if h.HasPayload && off < PacketSize {
		h.PayloadOff = off
	} else {
		h.PayloadOff = -1
		h.HasPayload = false
	}
	return h, nil
}

// TruncationError reports an input that ended mid-packet.
type TruncationError struct {
	Offset  int64 // absolute byte offset where the partial packet starts
	Partial int   // number of dangling bytes (1..187)
}

func (e *TruncationError) Error() string {
	return fmt.Sprintf("ts: truncated packet at byte %d (%d dangling bytes)", e.Offset, e.Partial)
}

// Reader frames a byte stream into 188-byte packets across arbitrary
// short reads. A trailing partial packet yields TruncationError exactly
// once; the dangling bytes are never mistaken for a valid packet.
type Reader struct {
	r     io.Reader
	buf   [PacketSize]byte
	index uint64 // sequence number of the next packet
	base  int64  // absolute byte offset of buf start
}

// NewReader validates the configured packet size and returns a Reader.
func NewReader(r io.Reader, packetSize int) (*Reader, error) {
	if packetSize != PacketSize {
		return nil, ErrBadPacketSize
	}
	return &Reader{r: r}, nil
}

// Next reads exactly one packet. It returns io.EOF at a packet boundary
// and *TruncationError when the stream ends mid-packet.
func (r *Reader) Next(pkt *[PacketSize]byte) (uint64, error) {
	n, err := io.ReadFull(r.r, r.buf[:])
	idx := r.index
	switch {
	case err == nil:
		r.index++
		r.base += PacketSize
		copy(pkt[:], r.buf[:])
		return idx, nil
	case err == io.EOF: // clean boundary, zero dangling bytes
		return idx, io.EOF
	default: // io.ErrUnexpectedEOF or a real read error
		if err == io.ErrUnexpectedEOF {
			return idx, &TruncationError{Offset: r.base, Partial: n}
		}
		return idx, err
	}
}

// CCVerdict classifies one continuity-counter observation.
type CCVerdict int

const (
	CCOK        CCVerdict = iota // expected counter
	CCFirst                      // first packet seen on this PID
	CCDuplicate                  // legal repeat (same counter as previous)
	CCGap                        // counter jumped: packets were lost
)

// CCTracker tracks the 4-bit continuity counter per PID.
type CCTracker struct {
	last map[uint16]uint8
	ok   map[uint16]bool
}

func NewCCTracker() *CCTracker {
	return &CCTracker{last: make(map[uint16]uint8), ok: make(map[uint16]bool)}
}

// Check classifies cc for pid. Packets without payload do not advance the
// counter and are excluded from tracking. The returned gap is the number
// of missing counter steps (0 unless the verdict is CCGap).
func (t *CCTracker) Check(pid uint16, cc uint8, hasPayload bool) (CCVerdict, int) {
	if !hasPayload {
		return CCOK, 0
	}
	if !t.ok[pid] {
		t.ok[pid] = true
		t.last[pid] = cc
		return CCFirst, 0
	}
	exp := (t.last[pid] + 1) & 0x0F
	switch cc {
	case exp:
		t.last[pid] = cc
		return CCOK, 0
	case t.last[pid]:
		return CCDuplicate, 0 // legal repeat, counter must not advance
	default:
		gap := int((cc - exp) & 0x0F)
		t.last[pid] = cc
		return CCGap, gap
	}
}
