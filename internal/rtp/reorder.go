package rtp

import (
	"io"

	"github.com/archive/tsrestore/internal/timeline"
)

// seqMod is the modulus of the 16-bit RTP sequence number.
const seqMod uint32 = 1 << 16

// DefaultWindow is the maximum reorder distance tolerated. Packets arriving
// more than this many sequence numbers ahead of the release point are held;
// once the release point advances past them the missing numbers are declared
// gaps and the held packets are released in order.
const DefaultWindow uint32 = 64

// ReorderStats summarizes what the reorder layer observed on one rendition.
type ReorderStats struct {
	Received   int // RTP datagrams accepted (duplicates counted separately)
	Duplicates int // datagrams whose sequence number was already seen
	LateDrop   int // packets outside the reorder window, discarded as late
	BadRTCP    int // malformed RTCP datagrams, counted and skipped
	Gaps       int // distinct missing sequence numbers declared as gaps
	Flushed    int // packets released at end-of-stream
}

// ReorderBuffer restores RTP sequence order despite loss, duplication,
// reordering and 16-bit wraparound. It is single-goroutine; the pipeline owns
// one buffer per rendition and drives it sequentially.
type ReorderBuffer struct {
	windowSize uint32
	held       map[uint32]*Packet // extended sequence -> held packet
	next       uint32             // extended sequence number to release next
	started    bool
	stats      ReorderStats
}

// NewReorderBuffer creates a buffer with the default reorder window.
func NewReorderBuffer() *ReorderBuffer {
	return NewReorderBufferWindow(DefaultWindow)
}

// NewReorderBufferWindow creates a buffer with an explicit window size.
func NewReorderBufferWindow(w uint32) *ReorderBuffer {
	if w == 0 {
		w = DefaultWindow
	}
	return &ReorderBuffer{windowSize: w, held: make(map[uint32]*Packet)}
}

// seqDelta returns cur-base as a signed 32-bit distance, handling wraparound.
func seqDelta(base, cur uint32) int64 { return int64(int32(cur - base)) }

// Push accepts one parsed RTP packet and returns any packets that became
// releasable (in order, possibly after skipping declared gaps). Duplicates and
// out-of-window late packets are counted and dropped; Push never blocks.
func (b *ReorderBuffer) Push(p *Packet) []*Packet {
	b.stats.Received++
	seq := uint32(p.Sequence)
	if !b.started {
		b.started = true
		b.next = seq
		b.held[seq] = p
		return b.advance()
	}
	d := seqDelta(b.next, seq)
	switch {
	case d < 0:
		// Older than the current release point: a late packet or duplicate.
		b.stats.LateDrop++
		return nil
	case d > int64(b.windowSize):
		// Far beyond the window: treat as a late/replayed packet from another
		// cycle rather than buffering an enormous number of phantom gaps.
		b.stats.LateDrop++
		return nil
	}
	if _, exists := b.held[seq]; exists {
		b.stats.Duplicates++
		return nil
	}
	b.held[seq] = p
	return b.advance()
}

// advance releases every contiguous packet starting at the release point. If
// the next number is missing it only waits while a later held packet still
// lies inside the reorder window; once the window is exhausted the missing run
// is declared as gaps and release continues, so loss cannot stall the stream.
func (b *ReorderBuffer) advance() []*Packet {
	out := make([]*Packet, 0, len(b.held))
	for len(b.held) > 0 {
		p, ok := b.held[b.next]
		if ok {
			delete(b.held, b.next)
			out = append(out, p)
			b.next++
			continue
		}
		// Hole at next. Wait only while the nearest held packet is inside the
		// window; a gap of exactly windowSize+1 means the window is exhausted.
		nearest := b.nearestHeld()
		if nearest-b.next < b.windowSize {
			return out
		}
		b.stats.Gaps += int(nearest - b.next)
		b.next = nearest
	}
	return out
}

// nearestHeld returns the smallest held sequence number.
func (b *ReorderBuffer) nearestHeld() uint32 {
	var best uint32
	first := true
	for seq := range b.held {
		if first || seq < best {
			best = seq
			first = false
		}
	}
	return best
}

// Flush is called at clean end-of-stream. Remaining held packets are released
// in order; any missing numbers before them are counted as gaps once.
func (b *ReorderBuffer) Flush() []*Packet {
	out := make([]*Packet, 0, len(b.held))
	for len(b.held) > 0 {
		if _, ok := b.held[b.next]; !ok {
			b.stats.Gaps++
			b.next++
			continue
		}
		p := b.held[b.next]
		delete(b.held, b.next)
		out = append(out, p)
		b.next++
	}
	b.stats.Flushed += len(out)
	return out
}

// Stats returns a copy of the running counters.
func (b *ReorderBuffer) Stats() ReorderStats { return b.stats }

// Pending reports how many packets are currently held out of order.
func (b *ReorderBuffer) Pending() int { return len(b.held) }

// OrderedReader combines length-prefixed framing, RTCP extraction and RTP
// reordering behind a single Next call.
type OrderedReader struct {
	fr     *FrameReader
	buf    *ReorderBuffer
	queued []*Packet
	// OnSR is invoked for every accepted sender report (nil-safe optional).
	OnSR func(SenderReport)
}

// NewOrderedReader builds an ordered reader over a framed datagram stream.
func NewOrderedReader(r io.Reader) *OrderedReader {
	return &OrderedReader{fr: NewFrameReader(r), buf: NewReorderBuffer()}
}

// Next returns the next in-order RTP packet. At clean end-of-stream it returns
// (nil, io.EOF) after flushing the reorder window. Malformed RTP datagrams
// yield a classified error; malformed RTCP is counted and skipped so a bad
// report never interrupts the media stream.
func (o *OrderedReader) Next() (*Packet, error) {
	for len(o.queued) == 0 {
		dgram, err := o.fr.ReadDatagram()
		if err != nil {
			if err == io.EOF {
				o.queued = append(o.queued, o.buf.Flush()...)
				if len(o.queued) == 0 {
					return nil, io.EOF
				}
				break
			}
			return nil, err
		}
		switch {
		case IsRTP(dgram):
			p, perr := ParsePacket(dgram)
			if perr != nil {
				return nil, perr
			}
			o.queued = append(o.queued, o.buf.Push(p)...)
		case IsRTCP(dgram):
			sr, serr := ParseSenderReport(dgram)
			if serr != nil {
				o.buf.stats.BadRTCP++
				continue
			}
			if o.OnSR != nil {
				o.OnSR(*sr)
			}
		default:
			return nil, timeline.NewError(timeline.ClassCorrupt, "rtp.demux",
				"datagram is neither RTP nor RTCP", nil)
		}
	}
	p := o.queued[0]
	o.queued = o.queued[1:]
	return p, nil
}

// Stats exposes the reorder counters.
func (o *OrderedReader) Stats() ReorderStats { return o.buf.Stats() }
