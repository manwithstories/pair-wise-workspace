// Package rtpwrap provides a bounded RTP reorder and duplicate window.
package rtpwrap

import (
	"fmt"

	"github.com/pion/rtp"

	"mediqc/internal/media"
)

// Decision classifies one pushed RTP packet.
type Decision string

const (
	DecisionDelivered     Decision = "delivered"
	DecisionBuffered      Decision = "buffered"
	DecisionDuplicate     Decision = "duplicate"
	DecisionOutsideWindow Decision = "outside_window_gap"
)

// Result is the deterministic outcome of a push.
type Result struct {
	Decision Decision
	Ordered  []*rtp.Packet
	Sequence uint16
}

// Window is a bounded sequence window. Buffered state and duplicate memory are
// both O(depth); no per-stream history grows without limit.
type Window struct {
	depth        uint16
	next         uint16
	started      bool
	buf          map[uint16]*rtp.Packet
	recent       map[uint16]struct{}
	recentQueue  []uint16
	recentMemory int

	Duplicates    int `json:"duplicates"`
	OutsideWindow int `json:"outside_window"`
	Gaps          int `json:"gaps"`
}

// NewWindow creates a window retaining depth forward sequences.
func NewWindow(depth int) *Window {
	if depth <= 0 {
		depth = 32
	}
	if depth > 4096 {
		depth = 4096
	}
	return &Window{
		depth:        uint16(depth),
		buf:          make(map[uint16]*rtp.Packet, depth),
		recent:       make(map[uint16]struct{}, depth*2+1),
		recentMemory: depth*2 + 1,
	}
}

// PushBytes unmarshals and accepts one raw RTP datagram.
func (w *Window) PushBytes(b []byte) (Result, *media.Error) {
	pkt := &rtp.Packet{}
	if err := pkt.Unmarshal(b); err != nil {
		return Result{}, &media.Error{
			Code: media.CodeLengthInvalid, Component: "rtp.window",
			Message: fmt.Sprintf("invalid RTP packet: %v", err),
		}
	}
	return w.Push(pkt), nil
}

// Push accepts one parsed RTP packet and returns any newly contiguous packets.
func (w *Window) Push(pkt *rtp.Packet) Result {
	seq := pkt.SequenceNumber
	if !w.started {
		w.started = true
		w.next = seq + 1
		w.remember(seq)
		return Result{Decision: DecisionDelivered, Ordered: []*rtp.Packet{pkt}, Sequence: seq}
	}

	if _, dup := w.recent[seq]; dup {
		w.Duplicates++
		return Result{Decision: DecisionDuplicate, Sequence: seq}
	}
	if _, dup := w.buf[seq]; dup {
		w.Duplicates++
		return Result{Decision: DecisionDuplicate, Sequence: seq}
	}

	forward := seq - w.next
	if forward >= 32768 {
		// The recent-set check above handles actual repeats. Any other packet
		// behind next is late beyond the contiguous frontier and is a gap.
		w.OutsideWindow++
		w.Gaps++
		return Result{Decision: DecisionOutsideWindow, Sequence: seq}
	}

	if forward > w.depth {
		w.OutsideWindow++
		w.Gaps += int(forward)
		w.next = seq + 1
		w.remember(seq)
		return Result{Decision: DecisionOutsideWindow, Ordered: []*rtp.Packet{pkt}, Sequence: seq}
	}

	if forward == 0 {
		w.remember(seq)
		w.next = seq + 1
		res := Result{Decision: DecisionDelivered, Sequence: seq}
		res.Ordered = append(res.Ordered, pkt)
		res.Ordered = append(res.Ordered, w.drain()...)
		return res
	}

	w.buf[seq] = pkt
	return Result{Decision: DecisionBuffered, Sequence: seq}
}

// Flush skips any missing sequences and emits buffered packets in order.
func (w *Window) Flush() []*rtp.Packet {
	var out []*rtp.Packet
	for len(w.buf) > 0 {
		best := uint16(0)
		bestForward := uint16(65535)
		found := false
		for seq := range w.buf {
			f := seq - w.next
			if f < 32768 && f < bestForward {
				bestForward, best, found = f, seq, true
			}
		}
		if !found {
			break
		}
		w.Gaps += int(bestForward)
		w.next += bestForward
		pkt := w.buf[best]
		delete(w.buf, best)
		w.remember(best)
		w.next = best + 1
		out = append(out, pkt)
		out = append(out, w.drain()...)
	}
	return out
}

func (w *Window) drain() []*rtp.Packet {
	var out []*rtp.Packet
	for {
		pkt, ok := w.buf[w.next]
		if !ok {
			return out
		}
		delete(w.buf, w.next)
		w.remember(w.next)
		w.next++
		out = append(out, pkt)
	}
}

func (w *Window) remember(seq uint16) {
	w.recent[seq] = struct{}{}
	w.recentQueue = append(w.recentQueue, seq)
	if len(w.recentQueue) <= w.recentMemory {
		return
	}
	oldest := w.recentQueue[0]
	w.recentQueue = w.recentQueue[1:]
	// Only delete when no newer insertion re-added the same wrapped sequence.
	if _, buffered := w.buf[oldest]; !buffered {
		delete(w.recent, oldest)
	}
}

// Buffered returns the current bounded buffer occupancy.
func (w *Window) Buffered() int { return len(w.buf) }
