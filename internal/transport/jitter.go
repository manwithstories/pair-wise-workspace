package transport

import (
	"sort"

	"github.com/pion/rtp"
)

// ReorderWindow is a bounded, sequence-addressed buffer. It holds at most
// size packets ahead of the current head. When a packet arrives beyond that
// horizon the head jumps forward, so a permanently lost packet cannot stall
// the stream; the skipped positions are reported to the caller.
type ReorderWindow struct {
	size    int
	next    uint16
	started bool
	pending map[uint16]*rtp.Packet
	skipped []uint16
	horizon int
	tooOld  int
	flushed int
}

// NewReorderWindow returns a window that holds at most size packets.
func NewReorderWindow(size int) *ReorderWindow {
	if size <= 0 {
		size = 64
	}
	return &ReorderWindow{size: size, pending: make(map[uint16]*rtp.Packet)}
}

// Push inserts a packet and returns the packets that became deliverable in
// order. accepted=false means the packet was a duplicate, too old, or beyond
// the horizon and was therefore not buffered.
func (w *ReorderWindow) Push(pkt *rtp.Packet) (ready []*rtp.Packet, accepted bool) {
	if pkt == nil {
		return nil, false
	}
	seq := pkt.SequenceNumber
	if !w.started {
		w.started = true
		w.next = seq
	}
	if seqLess(seq, w.next) {
		w.tooOld++
		return nil, false
	}
	if _, dup := w.pending[seq]; dup {
		return nil, false
	}
	// Advance the head until the new packet fits inside the horizon.
	for int(seqDist(w.next, seq)) >= w.size {
		if _, ok := w.pending[w.next]; ok {
			break
		}
		w.skipped = append(w.skipped, w.next)
		w.next++
		w.horizon++
	}
	w.pending[seq] = pkt
	return w.drain(), true
}

func (w *ReorderWindow) drain() []*rtp.Packet {
	var out []*rtp.Packet
	for {
		pkt, ok := w.pending[w.next]
		if !ok {
			break
		}
		delete(w.pending, w.next)
		out = append(out, pkt)
		w.next++
	}
	return out
}

// Skipped returns head positions that were abandoned to keep the stream
// moving. They may still arrive later; such arrivals count as too old.
func (w *ReorderWindow) Skipped() []uint16 { return w.skipped }

// Pending returns the number of buffered out-of-order packets.
func (w *ReorderWindow) Pending() int { return len(w.pending) }

// TooOld returns how many packets arrived before the head.
func (w *ReorderWindow) TooOld() int { return w.tooOld }

// HorizonJumps returns how many times the head was forced forward.
func (w *ReorderWindow) HorizonJumps() int { return w.horizon }

// SortedPending returns buffered sequence numbers on the ring.
func (w *ReorderWindow) SortedPending() []uint16 {
	seqs := make([]uint16, 0, len(w.pending))
	for s := range w.pending {
		seqs = append(seqs, s)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqLess(seqs[i], seqs[j]) })
	return seqs
}

// Flush releases everything still buffered, in sequence order.
func (w *ReorderWindow) Flush() []*rtp.Packet {
	var out []*rtp.Packet
	for _, s := range w.SortedPending() {
		out = append(out, w.pending[s])
		delete(w.pending, s)
		w.flushed++
	}
	return out
}

// seqLess compares 16-bit sequence numbers with wrap-around.
func seqLess(a, b uint16) bool { return int16(a-b) < 0 }

// seqDist returns the forward distance from a to b on the 16-bit ring.
func seqDist(a, b uint16) int { return int(uint16(b - a)) }
