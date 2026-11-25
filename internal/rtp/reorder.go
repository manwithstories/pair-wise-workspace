// Package rtp reorders an RTP packet stream per SSRC: duplicates are
// eliminated, same-sequence/different-payload packets are quarantined as
// conflicts, out-of-order packets are reseated within an adaptive wait
// window, and burst loss becomes explicit gaps. Audio is never fabricated.
package rtp

import (
	"bytes"
	"fmt"

	"github.com/pion/rtp"

	"mediad/internal/fault"
)

// Gap marks a run of missing sequence numbers.
type Gap struct {
	Start       uint16 `json:"start"`
	Count       int    `json:"count"`
	Recoverable bool   `json:"recoverable"` // false once the wait window expired
}

// Conflict records a quarantined same-seq/different-payload packet.
type Conflict struct {
	Seq uint16 `json:"seq"`
}

// Stats summarizes one stream's reordering outcome.
type Stats struct {
	Duplicates int       `json:"duplicates"`
	Late       int       `json:"late"` // arrived after the window closed
	Conflicts  []Conflict `json:"conflicts,omitempty"`
	Gaps       []Gap     `json:"gaps,omitempty"`
}

// Reorderer handles one SSRC.
type Reorderer struct {
	baseWindow int
	maxWindow  int
	window     int

	buf      map[uint16]*rtp.Packet
	declared map[uint16]bool // seqs already emitted as gaps
	expect   uint16
	started  bool
	stats    Stats
}

// New builds a Reorderer with a base wait window (packets) and a hard cap.
func New(baseWindow, maxWindow int) *Reorderer {
	return &Reorderer{
		baseWindow: baseWindow,
		maxWindow:  maxWindow,
		window:     baseWindow,
		buf:        map[uint16]*rtp.Packet{},
		declared:   map[uint16]bool{},
	}
}

// AdjustForBandwidth adapts the wait window to the observed bandwidth: a
// weaker link implies more jitter, so the window grows (bounded by max);
// a healthy link shrinks it back toward the base.
func (r *Reorderer) AdjustForBandwidth(bps, referenceBps float64) {
	if bps <= 0 || referenceBps <= 0 {
		return
	}
	w := r.baseWindow
	if bps < referenceBps {
		w = int(float64(r.baseWindow) * referenceBps / bps)
	}
	if w > r.maxWindow {
		w = r.maxWindow
	}
	if w < r.baseWindow {
		w = r.baseWindow
	}
	r.window = w
}

// Window reports the current adaptive wait window.
func (r *Reorderer) Window() int { return r.window }

// Push feeds one packet in arrival order and returns packets that became
// emit-ready in sequence order, plus any fault raised (conflict packets are
// quarantined, not emitted).
func (r *Reorderer) Push(p *rtp.Packet) ([]*rtp.Packet, error) {
	seq := p.SequenceNumber
	if !r.started {
		r.started = true
		r.expect = seq
	}
	if r.declared[seq] {
		// Beyond the wait window: unrecoverable, dropped, never fabricated.
		r.stats.Late++
		r.markGapUnrecoverable(seq)
		return nil, nil
	}
	if prev, ok := r.buf[seq]; ok {
		if bytes.Equal(prev.Payload, p.Payload) {
			r.stats.Duplicates++ // identical duplicate: eliminated
			return nil, nil
		}
		r.stats.Conflicts = append(r.stats.Conflicts, Conflict{Seq: seq})
		return nil, fault.New(fault.KindConflict, "reorder",
			"ssrc %d seq %d: same sequence, different payload; quarantined", p.SSRC, seq)
	}
	r.buf[seq] = p
	return r.drain(false), nil
}

// Flush force-emits everything still buffered, declaring gaps where needed.
func (r *Reorderer) Flush() []*rtp.Packet { return r.drain(true) }

// Stats returns the accumulated statistics.
func (r *Reorderer) Stats() Stats { return r.stats }

func (r *Reorderer) drain(flush bool) []*rtp.Packet {
	var out []*rtp.Packet
	for {
		if p, ok := r.buf[r.expect]; ok {
			out = append(out, p)
			delete(r.buf, r.expect)
			r.expect++
			continue
		}
		if len(r.buf) == 0 {
			break
		}
		// Missing r.expect. Emit it as a gap only when the buffer overflowed
		// the wait window or we are flushing.
		if !flush && len(r.buf) <= r.window {
			break
		}
		next, ok := r.lowestBuffered()
		if !ok {
			break
		}
		// Declare every seq in [expect, next) a gap.
		for s := r.expect; s != next; s++ {
			r.declared[s] = true
			r.stats.Gaps = append(r.stats.Gaps, Gap{Start: s, Count: 1, Recoverable: true})
		}
		r.expect = next
	}
	return out
}

func (r *Reorderer) lowestBuffered() (uint16, bool) {
	var best uint16
	found := false
	for s := range r.buf {
		if !found || seqLess(s, best) {
			best, found = s, true
		}
	}
	return best, found
}

func (r *Reorderer) markGapUnrecoverable(seq uint16) {
	for i := range r.stats.Gaps {
		g := &r.stats.Gaps[i]
		if g.Count == 1 && g.Start == seq {
			g.Recoverable = false
			return
		}
	}
}

func seqLess(a, b uint16) bool { return int16(a-b) < 0 }

// MergeGaps coalesces adjacent single-packet gaps for reporting.
func MergeGaps(gaps []Gap) []Gap {
	var out []Gap
	for _, g := range gaps {
		if n := len(out); n > 0 && out[n-1].Recoverable == g.Recoverable &&
			uint16(out[n-1].Start+uint16(out[n-1].Count)) == g.Start {
			out[n-1].Count += g.Count
			continue
		}
		out = append(out, g)
	}
	return out
}

func (s Stats) String() string {
	return fmt.Sprintf("dup=%d late=%d conflicts=%d gaps=%d",
		s.Duplicates, s.Late, len(s.Conflicts), len(s.Gaps))
}
