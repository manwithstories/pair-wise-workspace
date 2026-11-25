package meta

import (
	"encoding/binary"
	"math"

	"relaycheck/internal/media"
)

// MaxTrackBuffer is the streaming ceiling for one track's in-memory buffer.
const MaxTrackBuffer = 64 << 20 // 64 MiB

// SampleBudget enforces the per-track buffer ceiling while streaming samples.
type SampleBudget struct {
	TrackID int64
	Limit   int64
	used    int64
}

// NewSampleBudget builds a budget capped at MaxTrackBuffer for a track.
func NewSampleBudget(trackID int64) *SampleBudget {
	return &SampleBudget{TrackID: trackID, Limit: MaxTrackBuffer}
}

// Used reports currently reserved bytes.
func (b *SampleBudget) Used() int64 { return b.used }

// Reserve accounts for n bytes of one sample/chunk. It rejects negative
// lengths, a single oversized sample and cumulative use past the cap.
func (b *SampleBudget) Reserve(n int64) *media.Fault {
	if n < 0 {
		return media.NewFault(media.CodeOverflow, "negative sample length %d", n).WithTrack(b.TrackID)
	}
	if n > b.Limit {
		return media.NewFault(media.CodeBufferLimit, "sample of %d bytes exceeds per-track buffer limit %d", n, b.Limit).
			WithTrack(b.TrackID)
	}
	if b.used > b.Limit-n {
		return media.NewFault(media.CodeBufferLimit, "track buffer would exceed %d bytes (used=%d add=%d)", b.Limit, b.used, n).
			WithTrack(b.TrackID)
	}
	b.used += n
	return nil
}

// Release returns bytes after a sample has been flushed downstream.
func (b *SampleBudget) Release(n int64) {
	if n <= 0 {
		return
	}
	if n > b.used {
		b.used = 0
		return
	}
	b.used -= n
}

type boxHeader struct {
	typ        string
	headerSize int
	size       uint64
}

// decodeBoxHeader safely decodes an ISO-BMFF box header at off. It rejects
// short headers, impossible sizes and declared ranges outside data.
func decodeBoxHeader(data []byte, off int) (boxHeader, *media.Fault) {
	if off < 0 || off > len(data) || len(data)-off < 8 {
		return boxHeader{}, media.NewFault(media.CodeParseError, "short box header at offset %d", off)
	}
	size32 := binary.BigEndian.Uint32(data[off : off+4])
	h := boxHeader{typ: string(data[off+4 : off+8]), headerSize: 8}
	switch size32 {
	case 0: // box extends to end of file
		h.size = uint64(len(data) - off)
	case 1: // 64-bit largesize
		if len(data)-off < 16 {
			return boxHeader{}, media.NewFault(media.CodeParseError, "box %q largesize truncated at %d", h.typ, off)
		}
		h.size = binary.BigEndian.Uint64(data[off+8 : off+16])
		h.headerSize = 16
	default:
		h.size = uint64(size32)
	}
	if h.size < uint64(h.headerSize) {
		return boxHeader{}, media.NewFault(media.CodeParseError, "box %q size %d smaller than header %d", h.typ, h.size, h.headerSize)
	}
	avail := uint64(len(data) - off)
	if h.size > avail {
		return boxHeader{}, media.NewFault(media.CodeParseError, "box %q declares %d bytes but only %d remain at %d", h.typ, h.size, avail, off)
	}
	return h, nil
}

// containerBoxes are recursed into by the safety scanner.
var containerBoxes = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true,
	"edts": true, "udta": true, "dinf": true, "mvex": true, "moof": true,
	"traf": true, "mfra": true, "skip": false,
}

// scanBoxesSafe validates every box header in range and inspects stsz/mdat.
func scanBoxesSafe(data []byte) *media.Fault {
	return scanRangeSafe(data, 0, len(data), 0)
}

func scanRangeSafe(data []byte, start, end, depth int) *media.Fault {
	if depth > 40 {
		return media.NewFault(media.CodeParseError, "box nesting too deep")
	}
	off := start
	for off < end {
		h, f := decodeBoxHeader(data, off)
		if f != nil {
			return f
		}
		bodyStart := off + h.headerSize
		bodyEnd := off + int(h.size) // safe: h.size <= end-off <= len(data)
		if h.typ == "stsz" {
			if f := checkStsz(data[bodyStart:bodyEnd]); f != nil {
				return f
			}
		}
		if h.typ == "mdat" {
			payload := h.size - uint64(h.headerSize)
			if payload > uint64(MaxTrackBuffer) {
				return media.NewFault(media.CodeBufferLimit, "mdat payload %d exceeds buffer limit %d", payload, MaxTrackBuffer)
			}
		}
		if containerBoxes[h.typ] {
			if f := scanRangeSafe(data, bodyStart, bodyEnd, depth+1); f != nil {
				return f
			}
		}
		off = bodyEnd
	}
	return nil
}

// checkStsz validates a Sample Size Box payload without trusting count*width.
func checkStsz(p []byte) *media.Fault {
	if len(p) < 12 {
		return media.NewFault(media.CodeParseError, "stsz payload too short: %d", len(p))
	}
	uniform := binary.BigEndian.Uint32(p[4:8])
	count := binary.BigEndian.Uint32(p[8:12])
	if uniform != 0 {
		if uint64(uniform) > uint64(MaxTrackBuffer) {
			return media.NewFault(media.CodeBufferLimit, "uniform sample size %d exceeds limit", uniform)
		}
		total := uint64(uniform) * uint64(count)
		if total > math.MaxInt64 {
			return media.NewFault(media.CodeOverflow, "stsz total size overflows: %d*%d", uniform, count)
		}
		return nil
	}
	need64 := uint64(12) + uint64(count)*4
	if uint64(count) > (math.MaxUint32/4) || need64 > uint64(len(p)) {
		return media.NewFault(media.CodeParseError, "stsz sample count %d overflows/truncates payload len %d", count, len(p))
	}
	for i := uint32(0); i < count; i++ {
		sz := binary.BigEndian.Uint32(p[12+4*i : 16+4*i])
		if uint64(sz) > uint64(MaxTrackBuffer) {
			return media.NewFault(media.CodeBufferLimit, "stsz sample %d size %d exceeds limit", i, sz)
		}
	}
	return nil
}
