package rtp

import (
	"sort"
)

// Receiver reorders arrivals into a contiguous, gap-aware stream. It keeps a
// bounded window: packets older than the current output position are
// duplicates or too-late reorderings and are discarded, never buffered.
type Receiver struct {
	window int
	next   uint16
	have   bool
	buf    map[uint16]Packet
	seen   map[uint16]struct{} // every sequence accepted, for duplicate detection
	stats  ReceiverStats
}

// ReceiverStats are the counters the report needs.
type ReceiverStats struct {
	Received    int `json:"received"`
	Duplicate   int `json:"duplicate"`
	Reordered   int `json:"reordered"`
	Late        int `json:"late"`
	Lost        int `json:"lost"`
	Delivered   int `json:"delivered"`
	MaxBuffered int `json:"max_buffered"`
}

// NewReceiver starts at firstSeq and allows at most window out-of-order
// packets before declaring a gap lost and moving on.
func NewReceiver(firstSeq uint16, window int) *Receiver {
	if window <= 0 {
		window = 1
	}
	return &Receiver{
		window: window, next: firstSeq,
		buf: make(map[uint16]Packet), seen: make(map[uint16]struct{}),
	}
}

// Push feeds one arrival. It returns any packets that became deliverable in
// order. Expired packets (older than next) are counted and dropped.
func (r *Receiver) Push(p Packet) []Packet {
	r.stats.Received++
	if !r.have {
		r.next = p.Seq
		r.have = true
	}
	if r.buf == nil {
		r.buf = make(map[uint16]Packet)
	}
	if r.seen == nil {
		r.seen = make(map[uint16]struct{})
	}
	// A sequence we already accepted is a duplicate no matter how late.
	if _, dup := r.seen[p.Seq]; dup {
		r.stats.Duplicate++
		return nil
	}
	// Anything strictly before next but never seen is a too-late packet.
	if p.Seq != r.next && seqLT(p.Seq, r.next) {
		r.stats.Late++
		r.seen[p.Seq] = struct{}{}
		return nil
	}
	if p.Seq != r.next {
		// Out of order at accept time: it will be played later.
		r.stats.Reordered++
	}
	r.seen[p.Seq] = struct{}{}
	r.buf[p.Seq] = p
	if len(r.buf) > r.stats.MaxBuffered {
		r.stats.MaxBuffered = len(r.buf)
	}
	return r.drain(false)
}

// drain emits consecutive packets starting at next. When force is true (the
// window expired) it skips a missing head by advancing one slot and counts
// the hole as lost.
func (r *Receiver) drain(force bool) []Packet {
	var out []Packet
	for {
		if p, ok := r.buf[r.next]; ok {
			delete(r.buf, r.next)
			out = append(out, p)
			r.next++
			r.stats.Delivered++
			continue
		}
		if !force || len(r.buf) == 0 {
			break
		}
		// Find the smallest buffered sequence ahead of next; everything in
		// between is lost.
		var ahead []uint16
		for s := range r.buf {
			if seqLT(r.next, s) {
				ahead = append(ahead, s)
			}
		}
		if len(ahead) == 0 {
			break
		}
		sort.Slice(ahead, func(i, j int) bool { return seqLT(ahead[i], ahead[j]) })
		gap := seqDiff(r.next, ahead[0])
		r.stats.Lost += gap
		r.next = ahead[0]
	}
	return out
}

// Expire forces the window to close, delivering every buffered packet that
// can still be ordered and counting the gaps as lost. Packets older than the
// window are discarded.
func (r *Receiver) Expire() []Packet {
	out := r.drain(true)
	// Drop anything still buffered that is behind next (should not happen)
	// and count it as late.
	for s, p := range r.buf {
		if seqLT(s, r.next) {
			r.stats.Late++
			delete(r.buf, s)
			_ = p
		}
	}
	return out
}

// Buffered reports the current window occupancy.
func (r *Receiver) Buffered() int { return len(r.buf) }

// Stats snapshots counters. Received is the number of pushes; the
// categories are disjoint: duplicate + late + lost + delivered + reordered.
func (r *Receiver) Stats() ReceiverStats {
	return r.stats
}

// DeliverAll drives a whole arrival list through the receiver, expiring at
// the end so trailing holes are counted, and returns the ordered stream.
func DeliverAll(arrivals []Packet, window int) ([]Packet, ReceiverStats) {
	if len(arrivals) == 0 {
		return nil, ReceiverStats{}
	}
	r := NewReceiver(arrivals[0].Seq, window)
	// The receiver must start at the lowest sequence actually sent, not the
	// first arrival, otherwise an early swap would look like a huge gap.
	low := arrivals[0].Seq
	for _, p := range arrivals {
		if seqLT(p.Seq, low) {
			low = p.Seq
		}
	}
	r.next = low
	var out []Packet
	for _, p := range arrivals {
		out = append(out, r.Push(p)...)
	}
	out = append(out, r.Expire()...)
	return out, r.Stats()
}
