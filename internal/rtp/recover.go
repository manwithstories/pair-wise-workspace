// Package rtp recovers ordered packet streams from a transport that
// duplicates, reorders and burst-drops RTP packets. The reorder wait
// window adapts to a varying bandwidth trace; losses that outlive the
// window are recorded as retryable burst gaps. 16-bit sequence wrap is
// handled via RFC 3550 extended sequence numbers.
package rtp

import (
	"github.com/pion/rtp"

	"archive/internal/errs"
)

// Gap is a run of consecutively lost extended sequence numbers that
// outlived the wait window. Gaps are retryable: the caller may re-request
// them from the source.
type Gap struct {
	Start uint32 // first lost extended sequence number
	End   uint32 // last lost extended sequence number (inclusive)
}

// Len returns the burst length.
func (g Gap) Len() int { return int(g.End-g.Start) + 1 }

// Stats summarizes a recovery run.
type Stats struct {
	Received   int   // packets fed (including duplicates)
	Duplicates int   // exact duplicates dropped
	Emitted    int   // unique packets emitted in order
	Reordered  int   // emitted packets that had arrived out of order
	Lost       int   // packets declared lost after window expiry
	Gaps       []Gap // retryable burst-loss gaps
}

// RecoveryRate is the fraction of the expected stream that was emitted.
func (s Stats) RecoveryRate() float64 {
	if s.Emitted+s.Lost == 0 {
		return 1
	}
	return float64(s.Emitted) / float64(s.Emitted+s.Lost)
}

// Recoverer is a streaming reorder/recovery buffer.
type Recoverer struct {
	window  int
	buf     map[uint32]*rtp.Packet
	next    uint32 // next extended seq to emit
	maxExt  uint32 // highest extended seq seen
	init    bool
	Out     []*rtp.Packet // emitted packets, in order
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
	return &Recoverer{window: windowFor(mbps), buf: map[uint32]*rtp.Packet{}}
}

// SetBandwidth adjusts the wait window for the current link bandwidth.
func (r *Recoverer) SetBandwidth(mbps float64) { r.window = windowFor(mbps) }

// Window returns the current wait window.
func (r *Recoverer) Window() int { return r.window }

// extend maps a 16-bit sequence number to its 64k-cycle-extended value
// (RFC 3550 appendix A), tolerating wrap-around in both directions.
func (r *Recoverer) extend(seq uint16) uint32 {
	if !r.init {
		r.init = true
		r.next, r.maxExt = uint32(seq), uint32(seq)
		return uint32(seq)
	}
	maxSeq := uint16(r.maxExt)
	cycles := r.maxExt & 0xffff0000
	switch d := int32(seq) - int32(maxSeq); {
	case d > 0x8000: // very late packet from the previous cycle
		if cycles >= 0x10000 {
			cycles -= 0x10000
		}
	case d < -0x8000: // forward wrap
		cycles += 0x10000
	}
	ext := cycles | uint32(seq)
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
		r.stats.Duplicates++
		return nil
	}
	if _, dup := r.buf[ext]; dup {
		r.stats.Duplicates++
		return nil
	}
	if ext == r.next && len(r.buf) == 0 {
		// fast path: in-order arrival, emitted straight through
		r.stats.Emitted++
		r.Out = append(r.Out, p)
		r.next++
		return nil
	}
	r.buf[ext] = p
	r.drain()
	return nil
}

// drain emits in-order packets and declares losses once the buffer holds
// more future packets than the wait window allows.
func (r *Recoverer) drain() {
	for {
		if p, ok := r.buf[r.next]; ok {
			delete(r.buf, r.next)
			r.emit(p, r.next)
			r.next++
			continue
		}
		if len(r.buf) >= r.window {
			r.declareLost(r.next)
			r.next++
			continue
		}
		return
	}
}

func (r *Recoverer) emit(p *rtp.Packet, ext uint32) {
	// emitted from the reorder buffer rather than the fast path: this
	// packet (or the stream around it) arrived out of order and was
	// recovered by waiting
	r.stats.Emitted++
	r.stats.Reordered++
	r.Out = append(r.Out, p)
}

func (r *Recoverer) declareLost(ext uint32) {
	r.stats.Lost++
	if n := len(r.stats.Gaps); n > 0 && r.stats.Gaps[n-1].End+1 == ext {
		r.stats.Gaps[n-1].End = ext
		return
	}
	r.stats.Gaps = append(r.stats.Gaps, Gap{Start: ext, End: ext})
}

// Flush ends the stream: everything still buffered is emitted in order,
// and holes up to the highest seen sequence number are declared lost.
func (r *Recoverer) Flush() {
	if !r.init {
		return
	}
	for r.next <= r.maxExt {
		if p, ok := r.buf[r.next]; ok {
			delete(r.buf, r.next)
			r.emit(p, r.next)
		} else {
			r.declareLost(r.next)
		}
		r.next++
	}
}

// Stats returns the accumulated recovery statistics.
func (r *Recoverer) Stats() Stats { return r.stats }
