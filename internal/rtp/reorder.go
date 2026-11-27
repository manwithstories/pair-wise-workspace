package rtp

import (
	"fmt"
	"sort"

	"archiver/internal/media"
)

// DefaultWindow is the reorder window size in packets.
const DefaultWindow = 128

// Stats counts reorder outcomes per Reorderer.
type Stats struct {
	Received    int
	Emitted     int
	Duplicates  int // idempotent drops
	Reordered   int // arrived behind a higher sequence number
	Lost        int // gap declared after the window slid past
	OutOfWindow int // arrived outside the window; recorded as error
}

// Reorderer restores per-SSRC packet order using 32-bit extended
// sequence numbers inside a bounded window. Memory is bounded by the
// window size times the number of active SSRCs.
type Reorderer struct {
	window  uint64
	streams map[uint32]*stream
	stats   Stats
}

type stream struct {
	started   bool
	maxExt    uint64 // highest extended sequence number seen
	nextEmit  uint64 // next extended sequence number eligible for emission
	buf       map[uint64]*Packet
	accounted map[uint64]bool // within window: true=emitted, false=declared lost
}

func NewReorderer(window uint32) *Reorderer {
	if window == 0 {
		window = DefaultWindow
	}
	return &Reorderer{window: uint64(window), streams: map[uint32]*stream{}}
}

// extendSeq maps a 16-bit sequence number onto the 64-bit extended
// axis, choosing the cycle closest to the current maximum.
func extendSeq(seq uint16, maxExt uint64) uint64 {
	base := maxExt &^ 0xffff
	best, bestDist := uint64(0), uint64(1)<<63
	for _, c := range [3]uint64{base, base + 1<<16, base - 1<<16} {
		if base == 0 && c == base-1<<16 {
			continue // avoid huge underflow value
		}
		cand := c + uint64(seq)
		d := cand - maxExt
		if cand < maxExt {
			d = maxExt - cand
		}
		if d < bestDist {
			best, bestDist = cand, d
		}
	}
	return best
}

// Push ingests one RTP packet. It returns the packets that became
// emit-eligible (in extended sequence order) and, when the packet fell
// outside the window, a recoverable error recording the anomaly.
func (r *Reorderer) Push(p *Packet) ([]*Packet, *media.PacketError) {
	r.stats.Received++
	s := r.streams[p.SSRC]
	if s == nil {
		s = &stream{buf: map[uint64]*Packet{}, accounted: map[uint64]bool{}}
		r.streams[p.SSRC] = s
	}
	var ext uint64
	fresh := !s.started
	if fresh {
		s.started = true
		ext = uint64(p.Seq)
		s.maxExt = ext
		s.nextEmit = ext
	} else {
		ext = extendSeq(p.Seq, s.maxExt)
	}
	p.ExtSeq = ext // always expose the extended sequence number
	if fresh {
		// First packet of the stream: baseline, no reorder statistics.
	} else if ext > s.maxExt {
		s.maxExt = ext
	} else if emitted, seen := s.accounted[ext]; seen {
		if emitted {
			r.stats.Duplicates++ // idempotent drop of a re-delivered packet
			return nil, nil
		}
		// Declared lost when the window slid past: it can no longer be
		// merged, so isolate it as an out-of-window error record.
		r.stats.OutOfWindow++
		return nil, outOfWindowErr(p, ext, s.maxExt, r.window)
	} else if _, dup := s.buf[ext]; dup {
		r.stats.Duplicates++
		return nil, nil
	} else {
		r.stats.Reordered++
	}
	if ext+r.window <= s.maxExt {
		// Outside the trailing edge of the window: isolate, never merge.
		r.stats.OutOfWindow++
		return nil, outOfWindowErr(p, ext, s.maxExt, r.window)
	}
	s.buf[ext] = p
	return r.drain(s), nil
}

// drain emits buffered packets in order; gaps older than the window
// are declared lost so emission never stalls.
func (r *Reorderer) drain(s *stream) []*Packet {
	var out []*Packet
	for {
		emitted := false
		if p, ok := s.buf[s.nextEmit]; ok {
			delete(s.buf, s.nextEmit)
			p.ExtSeq = s.nextEmit
			out = append(out, p)
			emitted = true
		} else if s.nextEmit+r.window <= s.maxExt {
			r.stats.Lost++
		} else {
			break
		}
		s.accounted[s.nextEmit] = emitted
		s.nextEmit++
	}
	r.prune(s)
	r.stats.Emitted += len(out)
	return out
}

// Flush emits everything still buffered at end of stream, declaring
// remaining gaps lost.
func (r *Reorderer) Flush() []*Packet {
	var out []*Packet
	ssrcs := make([]uint32, 0, len(r.streams))
	for id := range r.streams {
		ssrcs = append(ssrcs, id)
	}
	sort.Slice(ssrcs, func(i, j int) bool { return ssrcs[i] < ssrcs[j] })
	for _, id := range ssrcs {
		s := r.streams[id]
		for len(s.buf) > 0 {
			if p, ok := s.buf[s.nextEmit]; ok {
				delete(s.buf, s.nextEmit)
				p.ExtSeq = s.nextEmit
				out = append(out, p)
				r.stats.Emitted++
			} else {
				r.stats.Lost++
			}
			s.nextEmit++
		}
	}
	return out
}

// prune bounds the accounted-set memory to the active window.
func (r *Reorderer) prune(s *stream) {
	lo := uint64(0)
	if s.maxExt > r.window {
		lo = s.maxExt - r.window
	}
	for e := range s.accounted {
		if e < lo {
			delete(s.accounted, e)
		}
	}
}

func outOfWindowErr(p *Packet, ext, maxExt, window uint64) *media.PacketError {
	return &media.PacketError{
		Offset: p.Offset,
		Reason: fmt.Sprintf("packet outside reorder window: ext seq %d, window [%d,%d]",
			ext, maxExt-window+1, maxExt),
	}
}

func (r *Reorderer) Stats() Stats { return r.stats }
