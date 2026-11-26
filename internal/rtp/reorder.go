package rtp

import (
	"io"

	"github.com/archive/tsrestore/internal/timeline"
)

// seqMod is the modulus of the 16-bit RTP sequence number.
const seqMod uint32 = 1 << 16

// DefaultWindow is the maximum reorder distance tolerated. A missing number
// is declared a gap once the high-water accepted sequence moves more than this
// many packets beyond it; surviving packets are then released in order.
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
// reordering and 16-bit wraparound. Internally it works in an extended 32-bit
// sequence space (the raw 16-bit counter lifted onto a monotonic axis) so map
// keys never collide across a wrap. Single-goroutine; one buffer per rendition.
type ReorderBuffer struct {
	windowSize uint32
	held       map[uint32]*Packet // extended sequence -> held packet
	next       uint32             // extended sequence to release next
	high       uint32             // highest extended sequence ever accepted
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

// seqDelta returns cur-base as a signed distance in 16-bit sequence space.
func seqDelta(base, cur uint32) int64 { return int64(int32(cur - base)) }

// extendSeq lifts the raw 16-bit sequence number onto the cycle implied by the
// current release point, choosing the instance nearest to next.
func (b *ReorderBuffer) extendSeq(raw uint16) uint32 {
	cur := uint32(raw)
	delta := (cur - b.next) & (seqMod - 1)
	if delta >= seqMod/2 {
		delta -= seqMod
	}
	return b.next + uint32(delta)
}

// Push accepts one parsed RTP packet and returns any packets that became
// releasable (in order, possibly after skipping declared gaps). Duplicates and
// out-of-window late packets are counted and dropped; Push never blocks.
func (b *ReorderBuffer) Push(p *Packet) []*Packet {
	b.stats.Received++
	if !b.started {
		b.started = true
		seq := uint32(p.Sequence)
		b.next, b.high = seq, seq
		b.held[seq] = p
		return b.advance()
	}
	seq := b.extendSeq(p.Sequence)
	if seq < b.next {
		// Older than the release point: late packet or duplicate of released.
		b.stats.LateDrop++
		return nil
	}
	if _, exists := b.held[seq]; exists {
		b.stats.Duplicates++
		return nil
	}
	b.held[seq] = p
	if seq > b.high {
		b.high = seq
	}
	return b.advance()
}

// advance releases contiguous packets starting at the release point. When the
// next number is missing it waits only while the high-water mark stays within
// the reorder window; beyond that the missing run is declared as gaps so loss
// cannot stall the stream until end-of-stream.
func (b *ReorderBuffer) advance() []*Packet {
	out := make([]*Packet, 0, len(b.held))
	for len(b.held) > 0 {
		if p, ok := b.held[b.next]; ok {
			delete(b.held, b.next)
			out = append(out, p)
			b.next++
			continue
		}
		// Hole at next. The reorder window tolerates up to windowSize packets
		// arriving ahead of the missing one. Forward distance from the hole to
		// the high-water mark is high-(next-1); beyond windowSize the window
		// is spent and the missing run must be declared as gaps.
		if b.high+1-b.next <= b.windowSize {
			return out
		}
		nearest := b.nearestHeld()
		b.stats.Gaps += int(nearest - b.next)
		b.next = nearest
	}
	return out
}

// nearestHeld returns the smallest extended sequence currently held.
func (b *ReorderBuffer) nearestHeld() uint32 {
	best := b.high + 1
	for seq := range b.held {
		if seq >= b.next && seq < best {
			best = seq
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
