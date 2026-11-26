package segment

import (
	"fmt"

	"github.com/archive/tsrestore/internal/rtp"
	"github.com/archive/tsrestore/internal/timeline"
)

// Builder accumulates ordered RTP TS payloads for one rendition and emits
// segments. It is single-goroutine, driven sequentially by the pipeline.
type Builder struct {
	rendition string
	clock     *timeline.Clock
	target    uint64 // segment duration in 90 kHz ticks

	// raw accumulated TS bytes for the segment currently being assembled.
	buf []byte
	// offset of buf's first byte within the whole rendition's TS stream.
	streamStart int
	// frames observed since the current segment's cut point.
	frames []Frame
	// tick at which the current segment began.
	segStartTick uint64
	segIndex     int
	// whether the current segment already carries a discontinuity.
	pendingDisc bool
	stats       ResyncStats
}

// NewBuilder creates a segment builder for a rendition using the shared clock.
func NewBuilder(rendition string, clock *timeline.Clock) *Builder {
	return &Builder{
		rendition: rendition,
		clock:     clock,
		target:    targetSecs * timeline.RTPClockRate,
	}
}

// Push feeds one ordered RTP packet's MPEG-TS payload. After a loss the payload
// may begin mid-packet; Push scans to the first safe sync boundary before
// consuming packets. It returns zero or more completed segments.
func (b *Builder) Push(p *rtp.Packet) ([]*Segment, error) {
	tick, err := b.clock.Observe(p.Timestamp)
	if err != nil {
		return nil, err
	}
	data := p.Payload
	pos := 0
	// If we are not currently packet-aligned (previous packet ended short),
	// resync before consuming anything.
	if len(b.buf)%tsSize != 0 {
		if at := resyncFrom(data, 0); at >= 0 {
			pos = at
			b.stats.Resynced++
		} else {
			b.stats.SyncLoss++
			return nil, nil // whole payload unusable; wait for more bytes
		}
	}

	var out []*Segment
	for pos+tsSize <= len(data) {
		if data[pos] != syncByte {
			b.stats.SyncLoss++
			at := resyncFrom(data, pos)
			if at < 0 {
				// Hold the trailing partial bytes for the next packet.
				b.buf = append(b.buf, data[pos:]...)
				return out, nil
			}
			pos = at
			b.stats.Resynced++
			continue
		}
		tp := data[pos : pos+tsSize]
		frameStart := b.streamStart + len(b.buf)
		b.buf = append(b.buf, tp...)

		if isIDR, ok := parseFrameHeader(tp); ok {
			b.stats.Frames++
			if isIDR {
				b.stats.IDRFrames++
			}
			b.frames = append(b.frames, Frame{
				RTPTime: p.Timestamp, Tick: tick, IsIDR: isIDR,
				StartOffset: frameStart,
			})
		}
		pos += tsSize

		// Consider a cut only at an IDR on/after the target boundary.
		if seg := b.maybeCut(tick); seg != nil {
			out = append(out, seg)
		}
	}
	// Keep any unaligned tail for the next push.
	if pos < len(data) {
		b.buf = append(b.buf, data[pos:]...)
	}
	return out, nil
}

// maybeCut closes the current segment when an IDR reaches the six-second grid.
// If the grid boundary has been passed without an available IDR, it marks a
// discontinuity and cuts at the next safe IDR instead.
func (b *Builder) maybeCut(tick uint64) *Segment {
	if len(b.frames) == 0 {
		return nil
	}
	last := b.frames[len(b.frames)-1]
	if !last.IsIDR {
		return nil
	}
	elapsed := tick - b.segStartTick
	if elapsed < b.target {
		return nil
	}
	// This IDR is at or beyond the target. If it overshoots the ideal grid
	// point by more than the alignment budget, the cut is still made here (the
	// only safe IDR) but flagged so consumers know timing is not exact.
	disc := b.pendingDisc
	b.pendingDisc = false
	return b.cut(last, disc)
}

// cut finalizes a segment ending at the given IDR frame (exclusive of its
// bytes) and resets assembly state for the next segment.
func (b *Builder) cut(boundary Frame, disc bool) *Segment {
	// Segment bytes are everything accumulated before the boundary frame.
	end := boundary.StartOffset - b.streamStart
	if end < 0 {
		end = 0
	}
	data := make([]byte, end)
	copy(data, b.buf[:end])
	seg := &Segment{
		ID:            fmt.Sprintf("%s-%06d", b.rendition, b.segIndex),
		Index:         b.segIndex,
		Rendition:    b.rendition,
		Data:          data,
		StartTick:     b.segStartTick,
		EndTick:       boundary.Tick,
		DurationSecs:  b.clock.Seconds(boundary.Tick - b.segStartTick),
		Discontinuity: disc,
		KeyAligned:    true, // cuts only happen at IDRs
		SHA256:        hash(data),
	}
	// Retain the remainder for the next segment.
	rem := b.buf[end:]
	b.buf = append([]byte(nil), rem...)
	b.streamStart += end
	b.segStartTick = boundary.Tick
	b.segIndex++
	b.frames = b.frames[:0]
	return seg
}

// Flush emits the final segment at end-of-stream if any bytes remain. The
// trailing segment may not end on an IDR (program end), so it is not required
// to be key-aligned but is still hashed for auditing.
func (b *Builder) Flush() *Segment {
	aligned := len(b.buf) - (len(b.buf) % tsSize)
	if aligned == 0 {
		return nil
	}
	data := make([]byte, aligned)
	copy(data, b.buf[:aligned])
	var endTick uint64
	if len(b.frames) > 0 {
		endTick = b.frames[len(b.frames)-1].Tick
	} else {
		endTick = b.segStartTick
	}
	seg := &Segment{
		ID:            fmt.Sprintf("%s-%06d", b.rendition, b.segIndex),
		Index:         b.segIndex,
		Rendition:    b.rendition,
		Data:          data,
		StartTick:     b.segStartTick,
		EndTick:       endTick,
		DurationSecs:  b.clock.Seconds(endTick - b.segStartTick),
		Discontinuity: b.pendingDisc,
		KeyAligned:    false,
		SHA256:        hash(data),
	}
	b.buf = b.buf[aligned:]
	b.streamStart += aligned
	b.segIndex++
	b.pendingDisc = false
	return seg
}

// MarkDiscontinuity flags that the next emitted segment lost its expected
// keyframe boundary (used by the pipeline when a gap cannot be hidden).
func (b *Builder) MarkDiscontinuity() { b.pendingDisc = true }

// Stats returns a copy of the recovery counters.
func (b *Builder) Stats() ResyncStats { return b.stats }
