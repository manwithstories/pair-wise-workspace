// Package rtp implements bounded reordering for RTP sequence numbers.
//
// The reorderer never invents packets. When the reorder window is exceeded
// the missing sequence numbers are reported as a Gap and the buffer resumes
// at the nearest buffered packet, so downstream stages see an explicit hole
// instead of silently shifted media.
package rtp

import (
	"fmt"

	"mediarun/internal/model"
)

// MaxWindow bounds the reorder buffer. A value above half the sequence space
// would make "ahead" and "behind" ambiguous.
const MaxWindow = 4096

// halfSpace is the sequence-space midpoint used to tell ahead from behind.
const halfSpace = 1 << 15

// Packet is the subset of an RTP packet the pipeline needs.
type Packet struct {
	Sequence  uint16
	Timestamp uint32
	TrackID   uint32
	Marker    bool
	Payload   []byte
}

// Gap describes a contiguous run of sequence numbers that was never
// delivered.
type Gap struct {
	TrackID uint32
	From    uint16
	To      uint16
	Count   int
}

// Result is returned for every pushed packet.
type Result struct {
	Ordered   []Packet
	Gaps      []Gap
	Duplicate bool
	TooOld    bool
}

// Stats accumulates counters for diagnostics.
type Stats struct {
	Received   int
	Delivered  int
	Duplicates int
	TooOld     int
	Gaps       int
	GapPackets int
}

// Reorder is a single-track sequence reorderer. It is not safe for
// concurrent use.
type Reorder struct {
	track   uint32
	window  int
	started bool
	next    uint16
	buf     map[uint16]Packet
	recent  map[uint16]struct{}
	stats   Stats
}

// New builds a reorderer. The window must be in [1, MaxWindow].
func New(trackID uint32, window int) (*Reorder, error) {
	if window < 1 || window > MaxWindow {
		return nil, fmt.Errorf("%w: reorder window %d", model.ErrInvalid, window)
	}
	return &Reorder{
		track:  trackID,
		window: window,
		buf:    make(map[uint16]Packet, window),
		recent: make(map[uint16]struct{}, window*2),
	}, nil
}

// SetWindow adjusts the window for subsequent packets. Reducing the window
// does not discard already buffered packets; they are flushed on the next
// push or on Close.
func (r *Reorder) SetWindow(window int) error {
	if window < 1 || window > MaxWindow {
		return fmt.Errorf("%w: reorder window %d", model.ErrInvalid, window)
	}
	r.window = window
	if len(r.buf) > window {
		return nil
	}
	r.recent = make(map[uint16]struct{}, window*2)
	return nil
}

// Window reports the active window size.
func (r *Reorder) Window() int { return r.window }

// Pending reports how many packets are buffered out of order.
func (r *Reorder) Pending() int { return len(r.buf) }

// Stats returns a copy of the counters.
func (r *Reorder) Stats() Stats { return r.stats }

// Push accepts one packet. Delivered packets are always in sequence order.
func (r *Reorder) Push(p Packet) Result {
	r.stats.Received++
	if !r.started {
		r.started = true
		r.next = p.Sequence
		return r.accept(p)
	}

	dist := forwardDistance(r.next, p.Sequence)
	switch {
	case dist == 0:
		return r.accept(p)
	case dist >= halfSpace:
		// Behind the expected sequence: either a duplicate or a packet
		// that arrived too late to be ordered.
		if _, seen := r.recent[p.Sequence]; seen {
			r.stats.Duplicates++
			return Result{Duplicate: true}
		}
		r.stats.TooOld++
		return Result{TooOld: true, Duplicate: r.wasBuffered(p.Sequence)}
	case int(dist) <= r.window:
		if _, ok := r.buf[p.Sequence]; ok {
			r.stats.Duplicates++
			return Result{Duplicate: true}
		}
		r.buf[p.Sequence] = p
		out, gaps := r.drain()
		return Result{Ordered: out, Gaps: gaps}
	default:
		// Beyond the window: the wait for the hole is over. Deliver what
		// is already buffered inside the hole in order, report the
		// missing runs, then resume at this packet.
		out, gaps := r.advanceTo(p.Sequence)
		out = append(out, p)
		r.stats.Delivered++
		r.markSeen(p.Sequence)
		r.next = p.Sequence + 1
		more, moreGaps := r.drain()
		out = append(out, more...)
		gaps = append(gaps, moreGaps...)
		return Result{Ordered: out, Gaps: gaps}
	}
}

func (r *Reorder) wasBuffered(seq uint16) bool {
	_, ok := r.buf[seq]
	// A buffered packet that is now behind the window can never be
	// delivered; the duplicate is reported and the buffer copy dropped.
	delete(r.buf, seq)
	return ok
}

func (r *Reorder) accept(p Packet) Result {
	r.stats.Delivered++
	r.markSeen(p.Sequence)
	r.next = p.Sequence + 1
	out, gaps := r.drain()
	out = append([]Packet{p}, out...)
	return Result{Ordered: out, Gaps: gaps}
}

// drain emits contiguous buffered packets and, once the buffer is at
// capacity without a contiguous head, skips to the nearest buffered packet
// and records the hole as a gap.
func (r *Reorder) drain() ([]Packet, []Gap) {
	var out []Packet
	var gaps []Gap
	for len(r.buf) > 0 {
		if p, ok := r.buf[r.next]; ok {
			out = append(out, p)
			delete(r.buf, r.next)
			r.stats.Delivered++
			r.markSeen(r.next)
			r.next++
			continue
		}
		if len(r.buf) < r.window {
			break
		}
		target, found := r.nearest()
		if !found {
			break
		}
		missed := int(forwardDistance(r.next, target))
		gaps = append(gaps, r.gapTo(target, missed))
		r.next = target
	}
	return out, gaps
}

// advanceTo emits every buffered packet before limit in sequence order,
// turning each unstored run into a gap. It never fabricates a packet.
func (r *Reorder) advanceTo(limit uint16) ([]Packet, []Gap) {
	var out []Packet
	var gaps []Gap
	for forwardDistance(r.next, limit) > 0 {
		if p, ok := r.buf[r.next]; ok {
			out = append(out, p)
			delete(r.buf, r.next)
			r.stats.Delivered++
			r.markSeen(r.next)
			r.next++
			continue
		}
		target, found := r.nearest()
		if !found || forwardDistance(r.next, target) >= forwardDistance(r.next, limit) {
			gaps = append(gaps, r.gapTo(limit, int(forwardDistance(r.next, limit))))
			r.next = limit
			break
		}
		gaps = append(gaps, r.gapTo(target, int(forwardDistance(r.next, target))))
		r.next = target
	}
	return out, gaps
}

func (r *Reorder) nearest() (uint16, bool) {
	var best uint16
	bestDist := uint32(1 << 31)
	found := false
	for seq := range r.buf {
		if d := uint32(forwardDistance(r.next, seq)); d < bestDist {
			best, bestDist, found = seq, d, true
		}
	}
	return best, found
}

func (r *Reorder) gapTo(target uint16, count int) Gap {
	g := Gap{TrackID: r.track, From: r.next, To: target - 1, Count: count}
	r.stats.Gaps++
	r.stats.GapPackets += count
	return g
}

// markSeen records a delivered sequence and keeps the dedup map bounded.
func (r *Reorder) markSeen(seq uint16) {
	if len(r.recent) >= r.window*2 {
		r.recent = make(map[uint16]struct{}, r.window*2)
	}
	r.recent[seq] = struct{}{}
}

// Close flushes the remaining buffer, reporting holes before buffered
// packets as gaps. Close is idempotent.
func (r *Reorder) Close() Result {
	res := Result{}
	for len(r.buf) > 0 {
		target, found := r.nearest()
		if !found {
			break
		}
		if missed := int(forwardDistance(r.next, target)); missed > 0 {
			res.Gaps = append(res.Gaps, r.gapTo(target, missed))
		}
		r.next = target
		// Emit at least the nearest packet so every iteration makes
		// progress; then take any contiguous followers.
		for {
			p, ok := r.buf[r.next]
			if !ok {
				break
			}
			res.Ordered = append(res.Ordered, p)
			delete(r.buf, r.next)
			r.stats.Delivered++
			r.next++
		}
	}
	return res
}

// Empty reports whether no packets are buffered.
func (r *Reorder) Empty() bool { return len(r.buf) == 0 }

// forwardDistance returns how many sequence numbers separate from and to,
// treating the space as circular. 0 means equal.
func forwardDistance(from, to uint16) uint16 {
	return to - from
}
