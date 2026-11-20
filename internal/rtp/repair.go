package rtp

import (
	"archive/internal/model"
)

// streamState holds the per-SSRC reorder buffer. Memory is bounded by the
// window: once window future packets are held, any hole is declared lost.
type streamState struct {
	ssrc      uint32
	window    int
	buf       map[int64]model.Packet
	next      int64 // next extended sequence to emit
	maxExt    int64 // highest extended sequence observed
	seenMax   int64 // highest extended sequence ever accepted (for dup check)
	init      bool
	emitted   map[int64]struct{}
	emittedN  int
	fed       int
	peak      int
	dups      int
	reordered int
	lost      int64
}

func newStream(ssrc uint32, window int) *streamState {
	return &streamState{
		ssrc:    ssrc,
		window:  window,
		buf:     map[int64]model.Packet{},
		emitted: map[int64]struct{}{},
	}
}

// extend maps a 16-bit sequence to a signed-extended value by the
// nearest-cycle rule relative to the highest sequence seen.
func (s *streamState) extend(seq uint16) int64 {
	if !s.init {
		s.init = true
		s.next, s.maxExt, s.seenMax = int64(seq), int64(seq), int64(seq)
		return int64(seq)
	}
	ext := s.maxExt + int64(int16(seq-uint16(s.maxExt)))
	if ext > s.maxExt {
		s.maxExt = ext
	}
	if ext > s.seenMax {
		s.seenMax = ext
	}
	return ext
}

// Repairer is a streaming, bounded reorder/dedup/loss engine over one or
// more SSRCs. Recovered packets are delivered to Emit; loss ranges are
// collected for the report. Nothing is retained beyond the window.
type Repairer struct {
	window  int
	streams map[uint32]*streamState
	lost    []model.LostRange
	// Emit, if set, receives each in-order packet as it is released.
	Emit func(model.Packet)
}

// NewRepairer creates a Repairer with a bounded reorder window (packets).
func NewRepairer(window int) *Repairer {
	if window < 1 {
		window = 1
	}
	return &Repairer{window: window, streams: map[uint32]*streamState{}}
}

func (r *Repairer) stateFor(ssrc uint32) *streamState {
	st, ok := r.streams[ssrc]
	if !ok {
		st = newStream(ssrc, r.window)
		r.streams[ssrc] = st
	}
	return st
}

// Feed ingests one parsed RTP packet. Duplicates (same SSRC + extended
// sequence) are counted and dropped; reordering within the window is
// recovered; holes past the window become loss ranges.
func (r *Repairer) Feed(p model.Packet) {
	st := r.stateFor(p.SSRC)
	st.fed++
	ext := st.extend(p.Sequence)
	p.ExtSequence = ext

	if _, done := st.emitted[ext]; done {
		st.dups++
		return
	}
	if _, held := st.buf[ext]; held {
		st.dups++
		return
	}
	if ext < st.next {
		// Arrived after the slot was closed as a loss: too late.
		st.dups++
		return
	}
	if ext < st.maxExt {
		st.reordered++
	}
	st.buf[ext] = p
	if len(st.buf) > st.peak {
		st.peak = len(st.buf)
	}
	r.drain(st)
}

// drain releases every packet that can be released in order. A hole is
// only declared once this stream's own sequence spread (highest minus next)
// reaches the window. Using the per-stream spread — rather than the total
// buffer length — keeps multi-SSRC interleaving from causing false losses.
func (r *Repairer) drain(st *streamState) {
	for {
		if p, ok := st.buf[st.next]; ok {
			delete(st.buf, st.next)
			r.release(st, p)
			continue
		}
		if st.maxExt-st.next >= int64(st.window) {
			r.declareLost(st)
			continue
		}
		return
	}
}

func (r *Repairer) release(st *streamState, p model.Packet) {
	st.emitted[p.ExtSequence] = struct{}{}
	st.emittedN++
	if len(st.emitted) > 4*st.window { // bound the dup-detection history
		delete(st.emitted, p.ExtSequence-int64(2*st.window))
	}
	st.next = p.ExtSequence + 1
	if r.Emit != nil {
		r.Emit(p)
	}
}

func (r *Repairer) declareLost(st *streamState) {
	missing := st.next
	st.lost++
	if n := len(r.lost); n > 0 && r.lost[n-1].SSRC == st.ssrc && r.lost[n-1].End+1 == missing {
		r.lost[n-1].End = missing
	} else {
		r.lost = append(r.lost, model.LostRange{SSRC: st.ssrc, Start: missing, End: missing})
	}
	st.emitted[missing] = struct{}{}
	st.next = missing + 1
}

// Flush ends the streams: remaining buffered packets are emitted in order
// and trailing holes up to the highest seen sequence become loss ranges.
func (r *Repairer) Flush() {
	for _, st := range r.streams {
		for st.next <= st.maxExt {
			if p, ok := st.buf[st.next]; ok {
				delete(st.buf, st.next)
				r.release(st, p)
			} else {
				r.declareLost(st)
			}
		}
	}
}

// Stats is the recovery summary across all SSRCs.
type Stats struct {
	Received   int
	Duplicates int
	Reordered  int
	Lost       int64
	PeakBuffer int
	Emitted    int
	LostRanges []model.LostRange
}

// Stats returns a bounded-memory recovery summary.
func (r *Repairer) Stats() Stats {
	out := Stats{LostRanges: r.lost}
	for _, st := range r.streams {
		out.Received += st.fed
		out.Duplicates += st.dups
		out.Reordered += st.reordered
		out.Lost += st.lost
		out.Emitted += st.emittedN
		if st.peak > out.PeakBuffer {
			out.PeakBuffer = st.peak
		}
	}
	return out
}
