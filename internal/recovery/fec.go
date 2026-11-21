// Package recovery reconstructs missing packet gaps from the bounded jitter
// window: bursts of at most three packets are repaired by linear interpolation
// between neighboring PCM samples; longer gaps become flagged silence. The
// reconstructed timeline always keeps the declared sample count.
package recovery

import (
	"avcheck/internal/clock"
	"avcheck/internal/jitter"
)

// MaxInterpolateBurst is the largest consecutive gap repaired by interpolation.
const MaxInterpolateBurst = 3

// Stats counts recovery outcomes.
type Stats struct {
	MissingPackets   int64
	Interpolated     int64
	SilenceFilled    int64
	InterpolatedSamp int64
	SilenceSamp      int64
}

// Reassembler drives one SSRC's jitter window and fills its gaps.
type Reassembler struct {
	win       *jitter.Window
	framesPkt int
	channels  int
	trackID   uint32
	ssrc      uint32
	stats     Stats
	last     *clock.Block // last block emitted (real or synthesized)
	lastReal *clock.Block // last received block, always a valid interpolation anchor
	emitted  []*clock.Block // every block produced, in timeline order
}

// NewReassembler builds a reassembler. framesPkt is the fixed packet duration in
// sample frames (all packets of the stream share it).
func NewReassembler(ssrc, track uint32, channels, framesPkt, windowPackets int) *Reassembler {
	return &Reassembler{
		win:       jitter.NewWindow(windowPackets),
		framesPkt: framesPkt,
		channels:  channels,
		trackID:   track,
		ssrc:      ssrc,
	}
}

// Window exposes the underlying window so the caller can push RTP blocks.
func (r *Reassembler) Window() *jitter.Window { return r.win }

// Offer pushes one received block into the reorder window.
func (r *Reassembler) Offer(b *clock.Block) (jitter.Outcome, error) {
	return r.win.Push(b)
}

// Stats returns the recovery counters.
func (r *Reassembler) Stats() Stats { return r.stats }

// Emit reconstructs every position up to the first still-missing sequence and
// returns the resulting blocks. Gaps are filled immediately using the next
// buffered block as the interpolation anchor.
func (r *Reassembler) Emit() []*clock.Block {
	var out []*clock.Block
	for {
		nextSeq, _ := r.win.NextSeq()
		if _, ok := r.win.Lookup(nextSeq); ok {
			// Drain releases the whole contiguous run, not just the head.
			for _, b := range r.win.Drain() {
				out = append(out, b)
				r.emitted = append(r.emitted, b)
				r.last = b
				r.lastReal = b
			}
			continue
		}
		anchor, has := r.win.FirstBuffered()
		if !has || r.lastReal == nil {
			return out // nothing reconstructible yet
		}
		burst := jitter.SeqDistance(nextSeq, anchor.Seq)
		filled := r.fillGap(nextSeq, r.lastReal, anchor, burst)
		out = append(out, filled...)
		r.emitted = append(r.emitted, filled...)
		r.win.SlideTo(anchor.Seq) // advance past missing seqs, keep anchor
	}
}

// Flush emits all buffered data and synthesizes trailing missing packets up to
// the given sequence number (end-of-stream semantics). Trailing gaps have no
// later anchor, so they are always filled with silence.
// Flush drains any remaining reconstructible blocks and synthesizes trailing
// missing packets up to upToSeq. It returns only the blocks not previously
// returned by Emit (the new tail); the full history is available via Emitted.
func (r *Reassembler) Flush(upToSeq uint16) []*clock.Block {
	had := len(r.emitted)
	if _, started := r.win.NextSeq(); started {
		// Emit already appends its output to r.emitted; do not append twice.
		r.Emit()
		if r.last != nil {
			tick := r.last.EndTick()
			for seq := r.last.Seq + 1; jitter.SeqDistance(seq, upToSeq) > 0; seq++ {
				b := r.synthBlock(seq, tick, r.lastReal)
				r.emitted = append(r.emitted, b)
				tick += int64(r.framesPkt)
				r.last = b
			}
		}
		r.win.Flush(upToSeq)
	}
	return r.emitted[had:]
}

// Emitted returns every block produced so far, in timeline order.
func (r *Reassembler) Emitted() []*clock.Block { return r.emitted }

// fillGap synthesizes burst missing packets starting at firstSeq between two
// real anchor blocks. The left anchor may be several packets before firstSeq
// (already-emitted real blocks in between), so ticks derive from firstSeq.
func (r *Reassembler) fillGap(firstSeq uint16, left, anchor *clock.Block, burst int) []*clock.Block {
	r.stats.MissingPackets += int64(burst)
	interpolate := burst <= MaxInterpolateBurst
	out := make([]*clock.Block, 0, burst)
	tick := int64(firstSeq) * int64(r.framesPkt)
	seq := firstSeq
	for i := 0; i < burst; i++ {
		var b *clock.Block
		if interpolate {
			b = r.interpBlock(seq, tick, left, anchor)
		} else {
			b = r.synthBlock(seq, tick, left)
		}
		out = append(out, b)
		tick += int64(r.framesPkt)
		seq++
		r.last = b
	}
	return out
}

// interpBlock builds one block by linear interpolation between two real anchors.
func (r *Reassembler) interpBlock(seq uint16, tick int64,
	prev, next *clock.Block) *clock.Block {
	frames := r.framesPkt
	samples := make([]int32, frames*r.channels)
	span := next.StartTick - prev.StartTick
	lastFrame := prev.Frames() - 1
	for f := 0; f < frames; f++ {
		pos := tick - prev.StartTick + int64(f)
		for c := 0; c < r.channels; c++ {
			a := prev.Samples[lastFrame*r.channels+c]
			z := next.Samples[c]
			samples[f*r.channels+c] = lerp(a, z, pos, span)
		}
	}
	r.stats.Interpolated++
	r.stats.InterpolatedSamp += int64(frames)
	return &clock.Block{TrackID: r.trackID, SSRC: r.ssrc, Seq: seq,
		StartTick: tick, Channels: r.channels, Samples: samples,
		Source: clock.SourceInterpolated, ArrivalMS: prev.ArrivalMS}
}

// synthBlock builds one silence-filled block.
func (r *Reassembler) synthBlock(seq uint16, tick int64,
	prev *clock.Block) *clock.Block {
	frames := r.framesPkt
	r.stats.SilenceFilled++
	r.stats.SilenceSamp += int64(frames)
	return &clock.Block{TrackID: r.trackID, SSRC: r.ssrc, Seq: seq,
		StartTick: tick, Channels: r.channels,
		Samples: make([]int32, frames*r.channels),
		Source: clock.SourceSilence, ArrivalMS: prev.ArrivalMS}
}

// lerp returns the integer linear interpolation of a..z at position pos/span.
func lerp(a, z int32, pos, span int64) int32 {
	if span <= 0 {
		return a
	}
	return a + int32((int64(z-a)*pos)/span)
}
