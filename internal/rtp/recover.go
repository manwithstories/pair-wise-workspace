// Package rtp recovers ordered packet streams from a transport that
// duplicates, reorders and burst-drops RTP packets. The reorder wait
// window adapts to a varying bandwidth trace; losses that outlive the
// window are recorded as retryable burst gaps. 16-bit sequence wrap is
// handled with signed extended sequence numbers (nearest-cycle rule).
package rtp

import (
	"github.com/pion/rtp"

	"archive/internal/errs"
)

// Gap is a run of consecutively lost extended sequence numbers that
// outlived the wait window. Gaps are retryable: the caller may re-request
// them from the source. Values are relative to Stats.Origin.
type Gap struct {
	Start int64 // first lost extended sequence number
	End   int64 // last lost extended sequence number (inclusive)
}

// Len returns the burst length.
func (g Gap) Len() int { return int(g.End-g.Start) + 1 }

// Stats summarizes a recovery run.
type Stats struct {
	Received   int   // packets fed (including duplicates)
	Duplicates int   // exact duplicates of already emitted packets
	Emitted    int   // unique packets emitted in order
	Reordered  int   // packets that arrived after a higher sequence number
	Late       int   // packets that arrived after their slot was closed
	Lost       int   // packets declared lost after window expiry
	Gaps       []Gap // retryable burst-loss gaps (relative to Origin)
	Origin     int64 // extended sequence number of the first emitted packet
}

// RecoveryRate is the fraction of the expected stream that was emitted.
func (s Stats) RecoveryRate() float64 {
	if s.Emitted+s.Lost == 0 {
		return 1
	}
	return float64(s.Emitted) / float64(s.Emitted+s.Lost)
}

// Recoverer is a streaming reorder/recovery buffer. It holds the first
// window of packets before emitting anything so a late leading packet can
// still move the stream origin back without losing data.
type Recoverer struct {
	window  int
	floor   int // minimum window implied by the declared reorder depth
	buf     map[int64]*rtp.Packet
	next    int64          // next extended seq to emit
	maxExt  int64          // highest extended seq seen
	init    bool           // first packet seen (extension origin)
	started bool           // warm-up finished; emission may proceed
	closed  map[int64]bool // recently emitted seqs, for duplicate detection
	pruned  int64          // seq below which closed entries were dropped
	Out     []*rtp.Packet  // emitted packets, in order
	stats   Stats
}

// windowFor maps a bandwidth in Mbps to a wait window: higher bandwidth
// means more packets in flight, so a wider reorder window.
func windowFor(mbps float64) int {
	w := int(mbps*16 + 0.5)
	if w < 8 {
		w = 8
	}
	if w > 64 {
		w = 64
	}
	return w
}

// NewRecoverer creates a Recoverer whose wait window follows mbps.
func NewRecoverer(mbps float64) *Recoverer {
	return &Recoverer{window: windowFor(mbps), buf: map[int64]*rtp.Packet{}, closed: map[int64]bool{}}
}

// SetBandwidth adjusts the wait window for the current link bandwidth.
func (r *Recoverer) SetBandwidth(mbps float64) {
	r.window = windowFor(mbps)
	if r.window < r.floor {
		r.window = r.floor
	}
}

// SetReorderDepth raises the wait window floor to n packets so a declared
// reorder depth is tolerated even when the bandwidth trace is low.
func (r *Recoverer) SetReorderDepth(n int) {
	if n < 0 {
		n = 0
	}
	r.floor = n
	if r.window < n {
		r.window = n
	}
}

// Window returns the current wait window.
func (r *Recoverer) Window() int { return r.window }

// extend maps a 16-bit sequence number to a signed extended value using
// the nearest-cycle rule relative to the highest sequence seen. The
// result is negative for a leading packet that arrived after a higher
// sequence number, so the caller can still place it correctly.
func (r *Recoverer) extend(seq uint16) int64 {
	if !r.init {
		r.init = true
		r.next, r.maxExt = int64(seq), int64(seq)
		return int64(seq)
	}
	ext := r.maxExt + int64(int16(seq-uint16(r.maxExt)))
	if ext > r.maxExt {
		r.maxExt = ext
	}
	return ext
}

// Feed ingests one raw RTP datagram.
func (r *Recoverer) Feed(asset string, raw []byte) error {
	p := &rtp.Packet{}
	if err := p.Unmarshal(raw); err != nil {
		return errs.New(errs.KindPacket, asset, "rtp header", "malformed RTP packet", err)
	}
	r.stats.Received++
	ext := r.extend(p.Header.SequenceNumber)
	if ext < r.next {
		if r.closed[ext] {
			r.stats.Duplicates++
			return nil
		}
		if !r.started && r.next-ext <= int64(r.window) {
			// leading packet inside the warm-up window: move the origin
			// back so it is emitted first
			r.next = ext
		} else {
			r.stats.Late++
			return nil
		}
	}
	if _, dup := r.buf[ext]; dup {
		r.stats.Duplicates++
		return nil
	}
	if ext < r.maxExt {
		r.stats.Reordered++
	}
	if r.started && ext == r.next && len(r.buf) == 0 {
		// fast path: in-order arrival after warm-up
		r.emit(p, ext)
		return nil
	}
	r.buf[ext] = p
	if !r.started && r.maxExt-r.next >= int64(r.window) {
		r.started = true
		r.drain()
	}
	return nil
}

// drain emits in-order packets and declares losses once the buffer holds
// more future packets than the wait window allows.
func (r *Recoverer) drain() {
	for {
		if p, ok := r.buf[r.next]; ok {
			delete(r.buf, r.next)
			r.emit(p, r.next)
			continue
		}
		if len(r.buf) >= r.window {
			r.declareLost(r.next)
			continue
		}
		return
	}
}

func (r *Recoverer) emit(p *rtp.Packet, ext int64) {
	if r.stats.Emitted == 0 {
		r.stats.Origin = ext
	}
	r.stats.Emitted++
	r.Out = append(r.Out, p)
	r.closed[ext] = true
	r.next = ext + 1
	r.prune()
}

func (r *Recoverer) declareLost(ext int64) {
	r.stats.Lost++
	if n := len(r.stats.Gaps); n > 0 && r.stats.Gaps[n-1].End+1 == ext {
		r.stats.Gaps[n-1].End = ext
	} else {
		r.stats.Gaps = append(r.stats.Gaps, Gap{Start: ext, End: ext})
	}
	r.closed[ext] = true
	r.next = ext + 1
	r.prune()
}

// prune drops duplicate-detection history that can no longer be relevant.
func (r *Recoverer) prune() {
	keepFrom := r.next - int64(2*r.window)
	if r.pruned >= keepFrom || len(r.closed) < 4*r.window {
		return
	}
	for ext := range r.closed {
		if ext < keepFrom {
			delete(r.closed, ext)
		}
	}
	r.pruned = keepFrom
}

// Flush ends the stream: everything still buffered is emitted in order,
// and holes up to the highest seen sequence number are declared lost.
func (r *Recoverer) Flush() {
	if !r.init {
		return
	}
	r.started = true
	for r.next <= r.maxExt {
		if p, ok := r.buf[r.next]; ok {
			r.emit(p, r.next)
		} else {
			r.declareLost(r.next)
		}
	}
}

// Stats returns the accumulated recovery statistics.
func (r *Recoverer) Stats() Stats { return r.stats }
