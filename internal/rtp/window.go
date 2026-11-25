// Package rtpwin reorders RTP packets and recovers losses from a
// retransmission cache inside a bounded sequence window. Sequence
// numbers and timestamps are handled in their wrapped (16/32-bit)
// domains via the shared clock unwrap helpers.
package rtpwin

import (
	"github.com/pion/rtp"

	"mediaqa/internal/media"
)

// Stats counts packet outcomes.
type Stats struct {
	Received   int // packets pushed (including duplicates)
	Duplicates int // pushes for already-delivered sequence numbers
	Recovered  int // gaps filled from the retransmission cache
	Lost       int // gaps declared unrecoverable
	Delivered  int // packets emitted in order
}

// Window is a bounded reorder/recovery buffer for one RTP stream.
type Window struct {
	size     uint64
	cache    map[uint16]*rtp.Packet // retransmission cache, by raw seq
	cacheCap int
	expected uint64 // next extended sequence number to deliver
	started  bool
	buf      map[uint64]*rtp.Packet
	Stats    Stats
}

// New creates a window of size sequence numbers with a retransmission
// cache holding up to cacheCap packets.
func New(size, cacheCap int) *Window {
	return &Window{
		size:     uint64(size),
		cache:    map[uint16]*rtp.Packet{},
		cacheCap: cacheCap,
		buf:      map[uint64]*rtp.Packet{},
	}
}

// CacheAdd stores a packet in the bounded retransmission cache.
func (w *Window) CacheAdd(p *rtp.Packet) {
	if len(w.cache) >= w.cacheCap {
		// Evict deterministically: the lowest raw sequence number.
		var min uint16
		first := true
		for k := range w.cache {
			if first || k < min {
				min, first = k, true
			}
		}
		delete(w.cache, min)
	}
	w.cache[p.SequenceNumber] = p
}

// Push ingests one received packet and returns packets newly deliverable
// in sequence order.
func (w *Window) Push(p *rtp.Packet) []*rtp.Packet {
	w.Stats.Received++
	if !w.started {
		w.expected = uint64(p.SequenceNumber)
		w.started = true
	}
	ext := media.Unwrap16(p.SequenceNumber, w.expected)
	if ext < w.expected {
		w.Stats.Duplicates++
		return nil
	}
	if _, dup := w.buf[ext]; dup {
		w.Stats.Duplicates++
		return nil
	}
	w.buf[ext] = p
	// Bound memory: force-declare losses when the gap exceeds the window.
	for ext-w.expected >= w.size {
		if !w.recoverOne() {
			w.Stats.Lost++
		}
		w.expected++
	}
	return w.drain()
}

// recoverOne tries the retransmission cache for the expected sequence.
func (w *Window) recoverOne() bool {
	raw := uint16(w.expected)
	if p, ok := w.cache[raw]; ok {
		w.buf[w.expected] = p
		delete(w.cache, raw)
		w.Stats.Recovered++
		return true
	}
	return false
}

// drain emits contiguous packets starting at expected.
func (w *Window) drain() []*rtp.Packet {
	var out []*rtp.Packet
	for {
		if p, ok := w.buf[w.expected]; ok {
			out = append(out, p)
			delete(w.buf, w.expected)
			w.expected++
			w.Stats.Delivered++
			continue
		}
		if w.recoverOne() {
			continue
		}
		return out
	}
}

// Finish ends the stream: every still-missing sequence number up to the
// highest buffered one is declared lost; buffered packets are delivered.
func (w *Window) Finish() []*rtp.Packet {
	if !w.started {
		return nil
	}
	var hi uint64
	for ext := range w.buf {
		if ext > hi {
			hi = ext
		}
	}
	var out []*rtp.Packet
	for w.expected <= hi {
		if _, ok := w.buf[w.expected]; !ok {
			if !w.recoverOne() {
				w.Stats.Lost++
				w.expected++
				continue
			}
		}
		out = append(out, w.buf[w.expected])
		delete(w.buf, w.expected)
		w.Stats.Delivered++
		w.expected++
	}
	return out
}

// RecoveryPct returns the percentage of gaps that were recovered.
func (s Stats) RecoveryPct() float64 {
	total := s.Recovered + s.Lost
	if total == 0 {
		return 100
	}
	return 100 * float64(s.Recovered) / float64(total)
}
