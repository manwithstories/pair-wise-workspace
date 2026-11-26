package ts

import (
	"bytes"

	"avpipe/internal/media"
)

const (
	PacketSize = 188
	SyncByte   = 0x47
)

// Packet is a minimally decoded 188-byte MPEG-TS transport packet.
// Payload aliases the input byte slice; callers must retain the source bytes.
type Packet struct {
	Index             int
	Offset            int
	TEI               bool
	PUSI              bool
	Priority          bool
	PID               uint16
	ScramblingControl uint8
	AdaptationControl uint8
	CC                uint8

	HasAdaptation bool
	HasPayload    bool
	PayloadOffset int
	Payload       []byte

	DiscontinuityIndicator bool
	RandomAccessIndicator  bool
	HasPCR                 bool
	PCR27                  uint64
}

// Stats contains deterministic framing counters.
type Stats struct {
	PacketCount       int   `json:"packets"`
	FirstSyncOffset   int   `json:"first_sync_offset"`
	FirstDesyncOffset int   `json:"first_desync_offset"`
	ResyncCount       int   `json:"resync_count"`
	IncompleteTail    int   `json:"incomplete_tail"`
	IncompleteBytes   int   `json:"incomplete_bytes"`
	InputBytes        int64 `json:"input_bytes"`
}

// ParseStream frames fixed TS packets without inventing a packet for a short
// tail. The first mismatch on an expected 188-byte boundary is retained.
func ParseStream(data []byte) ([]Packet, Stats, error) {
	st := Stats{FirstSyncOffset: -1, FirstDesyncOffset: -1, InputBytes: int64(len(data))}
	first := bytes.IndexByte(data, SyncByte)
	if first < 0 {
		st.FirstDesyncOffset = 0
		return nil, st, media.NewError(media.CodeSync, 0, "no MPEG-TS sync byte 0x47 found")
	}
	st.FirstSyncOffset = first
	if first != 0 {
		st.FirstDesyncOffset = 0
	}

	packets := make([]Packet, 0, len(data)/PacketSize)
	off := first
	index := 0
	for off < len(data) {
		if len(data)-off < PacketSize {
			st.IncompleteTail = 1
			st.IncompleteBytes = len(data) - off
			break
		}
		if data[off] != SyncByte {
			if st.FirstDesyncOffset < 0 {
				st.FirstDesyncOffset = off
			}
			next := findNextSyncCandidate(data, off+1)
			if next < 0 {
				return packets, st, media.NewError(media.CodeSync, int64(off), "sync byte lost and no valid 188-byte candidate")
			}
			off = next
			st.ResyncCount++
			continue
		}
		p, err := ParsePacket(data[off:off+PacketSize], off, index)
		if err != nil {
			return packets, st, err
		}
		packets = append(packets, p)
		index++
		off += PacketSize
	}
	st.PacketCount = len(packets)
	return packets, st, nil
}

func findNextSyncCandidate(data []byte, start int) int {
	for i := start; i+PacketSize <= len(data); i++ {
		if data[i] != SyncByte {
			continue
		}
		// Two consecutive framed sync bytes are a conservative candidate.
		if i+PacketSize < len(data) && data[i+PacketSize] != SyncByte {
			continue
		}
		return i
	}
	return -1
}

// ParsePacket decodes one exactly-sized packet.
func ParsePacket(b []byte, offset, index int) (Packet, error) {
	if len(b) != PacketSize {
		return Packet{}, media.NewError(media.CodeIncomplete, int64(offset), "packet length=%d, want 188", len(b))
	}
	if b[0] != SyncByte {
		return Packet{}, media.NewError(media.CodeSync, int64(offset), "bad sync byte 0x%02x", b[0])
	}
	p := Packet{
		Index:             index,
		Offset:            offset,
		TEI:               b[1]&0x80 != 0,
		PUSI:              b[1]&0x40 != 0,
		Priority:          b[1]&0x20 != 0,
		PID:               uint16(b[1]&0x1f)<<8 | uint16(b[2]),
		ScramblingControl: b[3] >> 6,
		AdaptationControl: (b[3] >> 4) & 0x03,
		CC:                b[3] & 0x0f,
	}
	if p.AdaptationControl == 0 {
		return p, media.NewError(media.CodeConfig, int64(offset), "reserved adaptation_field_control=0")
	}
	pos := 4
	if p.AdaptationControl == 2 || p.AdaptationControl == 3 {
		p.HasAdaptation = true
		if pos >= PacketSize {
			return p, media.NewError(media.CodePSILength, int64(offset), "missing adaptation length")
		}
		length := int(b[pos])
		if length > PacketSize-pos-1 {
			return p, media.NewError(media.CodePSILength, int64(offset+pos), "adaptation length %d exceeds packet", length)
		}
		adaptStart := pos + 1
		if length > 0 {
			flags := b[adaptStart]
			p.DiscontinuityIndicator = flags&0x80 != 0
			p.RandomAccessIndicator = flags&0x40 != 0
			if flags&0x10 != 0 {
				pcrAt := adaptStart + 1
				if pcrAt+6 > adaptStart+length {
					return p, media.NewError(media.CodePSILength, int64(offset+pcrAt), "truncated PCR field")
				}
				p.HasPCR = true
				p.PCR27 = decodePCR(b[pcrAt : pcrAt+6])
			}
		}
		pos = adaptStart + length
	}
	if p.AdaptationControl == 1 || p.AdaptationControl == 3 {
		if pos >= PacketSize {
			return p, media.NewError(media.CodeIncomplete, int64(offset), "payload starts beyond packet")
		}
		p.HasPayload = true
		p.PayloadOffset = pos
		p.Payload = b[pos:]
	}
	return p, nil
}

func decodePCR(b []byte) uint64 {
	base := uint64(b[0])<<25 | uint64(b[1])<<17 | uint64(b[2])<<9 |
		uint64(b[3])<<1 | uint64(b[4])>>7
	ext := uint64(b[4]&0x01)<<8 | uint64(b[5])
	return base*300 + ext
}

// CCGap describes one continuity-counter discontinuity. Updating the expected
// counter after reporting prevents one jump from being reported repeatedly.
type CCGap struct {
	PID      uint16 `json:"pid"`
	Offset   int    `json:"offset"`
	Expected uint8  `json:"expected"`
	Got      uint8  `json:"got"`
}

type CCTracker struct {
	last map[uint16]uint8
	seen map[uint16]bool
	Gaps []CCGap
}

func NewCCTracker() *CCTracker {
	return &CCTracker{last: map[uint16]uint8{}, seen: map[uint16]bool{}}
}

func (t *CCTracker) Check(p Packet) *CCGap {
	if p.PID == 0x1fff {
		return nil
	}
	if !t.seen[p.PID] {
		t.seen[p.PID] = true
		t.last[p.PID] = p.CC
		return nil
	}
	expected := (t.last[p.PID] + 1) & 0x0f
	if p.CC == t.last[p.PID] {
		return nil // legal duplicate
	}
	if p.CC == expected {
		t.last[p.PID] = p.CC
		return nil
	}
	gap := CCGap{PID: p.PID, Offset: p.Offset, Expected: expected, Got: p.CC}
	t.Gaps = append(t.Gaps, gap)
	t.last[p.PID] = p.CC
	return &gap
}

func (t *CCTracker) GapCount() int { return len(t.Gaps) }
