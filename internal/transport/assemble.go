package transport

import (
	"bytes"

	"archivectl/internal/timeline"
)

// Segment is one reassembled fMP4 init or media segment. Broken segments are
// retained only in the gap ledger; their bytes are never returned as media.
type Segment struct {
	ID               uint32
	Data             []byte
	RTPTimestamp     uint32
	ExtendedTS       int64
	Complete         bool
	MissingChunkFrom int
	MissingChunkTo   int
}

type segmentBuilder struct {
	seg         Segment
	buf         bytes.Buffer
	nextIndex   uint16
	started     bool
	ended       bool
	broken      bool
	missingFrom int
	missingTo   int
}

// Assembler reconstructs fMP4 segments from ordered chunk payloads.
type Assembler struct {
	builders    map[uint32]*segmentBuilder
	MaxBuilders int
	MaxSegment  int
}

func NewAssembler() *Assembler {
	return &Assembler{builders: map[uint32]*segmentBuilder{}, MaxBuilders: 16, MaxSegment: 8 * 1024 * 1024}
}

// Feed consumes ordered packets and returns newly complete segments.
func (a *Assembler) Feed(packets []OrderedPacket) ([]Segment, error) {
	completed := make([]Segment, 0)
	for _, op := range packets {
		h, body, err := ParseChunkHeader(op.Packet.Payload)
		if err != nil {
			return nil, err
		}
		b := a.builders[h.SegmentID]
		if h.Flags&ChunkFlagStart != 0 || b == nil {
			if len(a.builders) >= a.MaxBuilders && b == nil {
				return nil, timeline.New(timeline.CodeWindowOverflow, "assemble chunk", "too many open segments")
			}
			b = &segmentBuilder{}
			b.seg.ID = h.SegmentID
			b.seg.RTPTimestamp = op.Packet.Timestamp
			b.seg.ExtendedTS = op.ExtendedTimestamp
			b.started = true
			b.nextIndex = h.Index
			a.builders[h.SegmentID] = b
		}
		if !b.started {
			continue
		}
		if h.Index != b.nextIndex {
			b.broken = true
			b.missingFrom = int(b.nextIndex)
			b.missingTo = int(h.Index) - 1
			b.seg.MissingChunkFrom = b.missingFrom
			b.seg.MissingChunkTo = b.missingTo
		}
		if b.buf.Len()+len(body) > a.MaxSegment {
			return nil, timeline.New(timeline.CodeOverflow, "assemble chunk", "segment exceeds bounded maximum")
		}
		_, _ = b.buf.Write(body)
		b.nextIndex = h.Index + 1
		if h.Flags&ChunkFlagEnd != 0 {
			b.ended = true
			if !b.broken {
				b.seg.Data = append([]byte(nil), b.buf.Bytes()...)
				b.seg.Complete = true
				completed = append(completed, b.seg)
			}
			delete(a.builders, h.SegmentID)
		}
	}
	return completed, nil
}

// AssembleCapture runs the full window and assembler over raw RTP frames.
func AssembleCapture(frames [][]byte, windowSize int) ([]Segment, *ReorderWindow, error) {
	w, err := NewReorderWindow(windowSize)
	if err != nil {
		return nil, nil, err
	}
	a := NewAssembler()
	segments := make([]Segment, 0)
	for _, f := range frames {
		ready, err := w.AcceptBytes(f)
		if err != nil {
			return nil, w, err
		}
		segs, err := a.Feed(ready)
		if err != nil {
			return nil, w, err
		}
		segments = append(segments, segs...)
	}
	final := w.Finish()
	segs, err := a.Feed(final)
	if err != nil {
		return nil, w, err
	}
	segments = append(segments, segs...)
	return segments, w, nil
}
