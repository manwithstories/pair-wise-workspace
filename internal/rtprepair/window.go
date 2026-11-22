// Package rtprepair deduplicates and reorders RTP packets inside a sliding
// window and explicitly marks gaps as lost; missing packets are never
// silently fabricated.
package rtprepair

import "github.com/pion/rtp"

// Stats counts what the window observed.
type Stats struct {
	Received   int // packets pushed, including duplicates
	Duplicates int // dropped duplicates
	Reordered  int // arrived behind the highest sequence seen so far
	Lost       int // declared missing after the window slid past
	Delivered  int // emitted in order
}

// Item is one drain result: either an in-order packet, or a loss mark
// (Packet == nil) listing sequence numbers declared lost.
type Item struct {
	Packet  *rtp.Packet
	Missing []uint16
}

// Window is a fixed-size reorder window over uint16 RTP sequence numbers.
type Window struct {
	size    int
	started bool
	next    uint16 // next sequence number to deliver
	maxSeen uint16
	buf     map[uint16]*rtp.Packet
	Stats   Stats
}

// New creates a window holding up to size out-of-order packets.
func New(size int) *Window {
	if size < 1 {
		size = 1
	}
	return &Window{size: size, buf: map[uint16]*rtp.Packet{}}
}

// behind reports whether a is behind b in circular uint16 order.
func behind(a, b uint16) bool { return int16(a-b) < 0 }

// Push inserts one packet and returns whatever became deliverable.
func (w *Window) Push(p *rtp.Packet) []Item {
	w.Stats.Received++
	seq := p.SequenceNumber
	if !w.started {
		w.started = true
		w.next = seq
		w.maxSeen = seq
	}
	if _, ok := w.buf[seq]; ok || behind(seq, w.next) {
		w.Stats.Duplicates++
		return w.drain()
	}
	if behind(seq, w.maxSeen) {
		w.Stats.Reordered++
	} else {
		w.maxSeen = seq
	}
	w.buf[seq] = p
	return w.drain()
}

// Flush drains the window at end of stream, declaring remaining gaps lost.
func (w *Window) Flush() []Item {
	var out []Item
	for len(w.buf) > 0 {
		out = append(out, w.step(true)...)
	}
	return out
}

func (w *Window) drain() []Item {
	var out []Item
	for {
		items := w.step(false)
		if len(items) == 0 {
			return out
		}
		out = append(out, items...)
	}
}

// step delivers w.next if buffered, or declares it lost when the window has
// slid past. force (Flush) declares any gap lost immediately.
func (w *Window) step(force bool) []Item {
	if p, ok := w.buf[w.next]; ok {
		delete(w.buf, w.next)
		w.next++
		w.Stats.Delivered++
		return []Item{{Packet: p}}
	}
	gapBeyondWindow := behind(w.next, w.maxSeen) &&
		uint16(w.maxSeen-w.next) >= uint16(w.size)
	if force || len(w.buf) >= w.size || gapBeyondWindow {
		lost := w.next
		w.next++
		w.Stats.Lost++
		return []Item{{Missing: []uint16{lost}}}
	}
	return nil
}
