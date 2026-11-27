// Package rtpadapt wraps pion/rtp to provide a bounded reorder window that
// deduplicates by extended sequence number, tolerates 16-bit wrap and reports
// late packets that arrive outside the window as explicit gaps.
package rtpadapt

import (
	"github.com/pion/rtp"
	"pairwise/ingestcheck/internal/clock"
)

// SeqSpace is the size of the RTP sequence space.
const SeqSpace = 1 << 16

// Packet is one received RTP datagram after unpacking. Ext is the extended
// sequence number computed across wrap boundaries so comparisons are total
// and repeatable.
type Packet struct {
	Ext      uint64
	Seq      uint16
	TS       uint32
	SSRC     uint32
	Marker   bool
	Payload  []byte
	Arrival  int64  // monotonic arrival tick supplied by the caller
	Fragment uint32 // source fragment index, for traceability
}

// Gap is a half-open range [Start, End) of extended sequence numbers that
// never arrived inside the reorder window.
type Gap struct {
	Start uint64
	End   uint64
}

// Stats accumulates delivery accounting for the audit trail.
type Stats struct {
	Received    int
	Emitted     int
	Duplicate   int
	Late        int
	Lost        int
	MaxHeldPeak int
	Gaps        []Gap
}

// Config bounds the window. Capacity is the maximum number of packets held
// for reordering; once exceeded the window gives up on the oldest hole so
// memory and waiting time stay bounded even under a burst.
//
// MaxSeen bounds the duplicate-detection horizon: an already-emitted
// sequence more than MaxSeen behind the drain head is treated as a late
// packet rather than a replay, which keeps the horizon (and thus the
// comparison work) fixed.
type Config struct {
	Capacity int
	MaxSeen  int
}

// DefaultConfig picks a window small enough that buffers stay bounded even
// under burst loss.
func DefaultConfig() Config { return Config{Capacity: 64, MaxSeen: 64} }

// Window accumulates packets by extended sequence number. Callers feed it in
// arrival order; it returns only packets ready to emit, in order.
type Window struct {
	cfg     Config
	started bool
	next    uint64            // first extended sequence not yet emitted
	held    map[uint64]Packet // received but not yet emitted, in-window
	stats   Stats
	spans   bool // whether a wrap has been observed (diagnostics only)
}

// New creates a window with the supplied bounds.
func New(cfg Config) (*Window, error) {
	if cfg.Capacity <= 0 || cfg.MaxSeen <= 0 {
		return nil, clock.New(clock.CodeArgument, "rtp", "non-positive window bound")
	}
	cfg.Capacity = min(cfg.Capacity, 1<<20)
	cfg.MaxSeen = min(cfg.MaxSeen, 1<<20)
	return &Window{cfg: cfg, held: make(map[uint64]Packet)}, nil
}

// Feed unpacks one datagram and records it, returning ready packets.
func (w *Window) Feed(datagram []byte, arrival int64, fragment uint32) ([]Packet, error) {
	var p rtp.Packet
	if err := p.Unmarshal(datagram); err != nil {
		return nil, clock.At(clock.CodeSequenceWindow, "rtp.unmarshal", 0, err.Error())
	}
	return w.FeedPacket(&p, arrival, fragment)
}

// FeedPacket records an already-unpacked packet, letting tests inject
// synthetic streams without re-marshalling.
func (w *Window) FeedPacket(p *rtp.Packet, arrival int64, fragment uint32) ([]Packet, error) {
	seq := uint64(p.SequenceNumber)
	if !w.started {
		w.next = seq
		w.started = true
	}
	w.stats.Received++

	// Signed modular distance from the drain head to this sequence. The
	// comparison is done on the low 16 bits, so a wrap is just a negative or
	// positive distance depending on which side of the boundary we are.
	d := signedDistance(seq, w.next)

	switch {
	case d < 0:
		// Already-emitted position, or a position the window has passed.
		if uint64(-d) <= uint64(w.cfg.MaxSeen) {
			w.stats.Duplicate++
			return nil, nil
		}
		w.stats.Late++
		// Record the position as a gap when it still lies in the positive
		// extended domain; otherwise it predates the anchor and is only
		// counted as late.
		back := uint64(-d)
		if w.next >= back {
			pos := w.next - back
			w.recordGap(pos, pos+1)
		}
		return nil, nil
	default:
		ext := w.next + uint64(d)
		if _, ok := w.held[ext]; ok {
			w.stats.Duplicate++
			return nil, nil
		}
		w.held[ext] = Packet{
			Ext:      ext,
			Seq:      p.SequenceNumber,
			TS:       p.Timestamp,
			SSRC:     p.SSRC,
			Marker:   p.Marker,
			Payload:  append([]byte(nil), p.Payload...),
			Arrival:  arrival,
			Fragment: fragment,
		}
		if ext >= SeqSpace {
			w.spans = true
		}
		if len(w.held) > w.stats.MaxHeldPeak {
			w.stats.MaxHeldPeak = len(w.held)
		}
		return w.drain(), nil
	}
}

// signedDistance returns the signed distance from the drain head's low 16
// bits to seq, in (-SeqSpace/2, SeqSpace/2]. The result is the position of
// seq relative to next when only 16-bit sequence numbers are visible.
func signedDistance(seq, next uint64) int64 {
	d := int64(seq&0xFFFF) - int64(next&0xFFFF)
	if d > SeqSpace/2 {
		d -= SeqSpace
	} else if d < -SeqSpace/2 {
		d += SeqSpace
	}
	return d
}

// Flush emits everything still held, recording holes in between. The caller
// invokes it once the source signals end-of-stream.
func (w *Window) Flush() []Packet {
	var out []Packet
	for len(w.held) > 0 {
		lo := minHeld(w.held)
		if lo > w.next {
			w.recordGap(w.next, lo)
			w.next = lo
		}
		emitted := w.drain()
		out = append(out, emitted...)
		if len(emitted) == 0 {
			// drain could not advance (defensive: should not happen once the
			// hole is recorded above).
			break
		}
	}
	return out
}

// Stats returns a copy of the current accounting, including the total number
// of lost sequence positions implied by the recorded holes.
func (w *Window) Stats() Stats {
	s := w.stats
	s.Gaps = append([]Gap(nil), w.stats.Gaps...)
	for _, g := range s.Gaps {
		s.Lost += int(g.End - g.Start)
	}
	return s
}

// drain emits packets contiguous from next. When the held queue exceeds
// Capacity it gives up on the oldest hole first, so memory and waiting time
// stay bounded regardless of how bursty the loss is.
func (w *Window) drain() []Packet {
	var out []Packet
	for len(w.held) > 0 {
		lo := minHeld(w.held)
		switch {
		case lo == w.next:
			pk := w.held[w.next]
			delete(w.held, w.next)
			w.next++
			w.stats.Emitted++
			out = append(out, pk)
		case lo > w.next && len(w.held) > w.cfg.Capacity:
			w.recordGap(w.next, lo)
			w.next = lo
		default:
			return out
		}
	}
	return out
}

func minHeld(m map[uint64]Packet) uint64 {
	lo := ^uint64(0)
	for ext := range m {
		if ext < lo {
			lo = ext
		}
	}
	return lo
}

// recordGap appends a merged gap range. Adjacent ranges coalesce so a burst
// loss produces a single entry per hole. The list is bounded by Capacity so
// the audit structure cannot grow without bound.
func (w *Window) recordGap(start, end uint64) {
	if start >= end {
		return
	}
	if n := len(w.stats.Gaps); n > 0 && w.stats.Gaps[n-1].End == start {
		w.stats.Gaps[n-1].End = end
		return
	}
	if len(w.stats.Gaps) >= w.cfg.Capacity {
		last := &w.stats.Gaps[len(w.stats.Gaps)-1]
		if last.End < end {
			last.End = end
		}
		return
	}
	w.stats.Gaps = append(w.stats.Gaps, Gap{Start: start, End: end})
}
