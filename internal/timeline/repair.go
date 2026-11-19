// Package timeline converts per-frame timestamps onto one absolute media
// timeline in 90 kHz ticks and repairs wrapping, duplicate sequence numbers,
// reordering and small backwards drift. Anything that cannot be repaired
// unambiguously is quarantined with a diagnosable reason.
package timeline

import (
	"fmt"
	"sort"

	"mediq/internal/model"
)

// Status is the repair outcome for one frame.
type Status int

const (
	StatusIndexed Status = iota
	StatusQuarantined
)

// Event describes the decision taken for one frame.
type Event struct {
	Frame    *model.Frame
	Status   Status
	Reason   string
	Fault    *model.Fault
	Media    int64 // 90 kHz ticks, absolute, strictly increasing
	Adjusted bool
}

// Options tunes the repair heuristics. Tolerances are in 90 kHz ticks.
type Options struct {
	// ReorderWindow bounds how many sequence gaps are buffered before the
	// repairer makes progress regardless. Default 32.
	ReorderWindow int
	// DriftTolerance is the largest backward movement treated as jitter and
	// clamped forward. Default 450 ticks (5 ms).
	DriftTolerance int64
	// WrapModulus is the modulus of the source capture clock. Default 2^32,
	// matching the 32-bit RTP timestamp field.
	WrapModulus uint64
}

func (o Options) withDefaults() Options {
	if o.ReorderWindow <= 0 {
		o.ReorderWindow = 32
	}
	if o.DriftTolerance <= 0 {
		o.DriftTolerance = 450
	}
	if o.WrapModulus == 0 {
		o.WrapModulus = 1 << 32
	}
	return o
}

// Repairer buffers a bounded out-of-order window and assigns media times.
// Its memory is O(ReorderWindow) regardless of input length.
type Repairer struct {
	opts Options

	started  bool
	nextSeq  uint64
	floorSeq uint64 // sequence number of the first observed frame
	pending  map[uint64]*model.Frame
	gaps     map[uint64]struct{} // sequence numbers known to be lost
	recent   map[uint64]uint64   // seq -> generation, pruned with nextSeq

	haveTick bool
	lastRaw  uint64
	lastTB   model.Timebase
	ticks    int64 // absolute source-clock ticks since the first frame

	haveMedia bool
	lastMedia int64
	lastDelta int64
	wraps     uint64
}

func New(opts Options) *Repairer {
	return &Repairer{
		opts:    opts.withDefaults(),
		pending: make(map[uint64]*model.Frame),
		gaps:    make(map[uint64]struct{}),
		recent:  make(map[uint64]uint64),
	}
}

// Wraps reports how many timestamp wraps were repaired.
func (r *Repairer) Wraps() uint64 { return r.wraps }

// Skip tells the repairer that seq will never arrive (it failed validation and
// was dropped). The reorder window can move past the gap immediately instead
// of stalling until the window fills, and later arrivals are reported as
// at-least-once duplicates rather than silently colliding.
func (r *Repairer) Skip(seq uint64) {
	if !r.started {
		return
	}
	if seq < r.nextSeq || seq > r.nextSeq+uint64(r.opts.ReorderWindow) {
		return
	}
	delete(r.pending, seq)
	if seq != r.nextSeq {
		return
	}
	r.nextSeq++
	for {
		if _, ok := r.pending[r.nextSeq]; ok {
			break
		}
		if _, gap := r.gaps[r.nextSeq]; gap {
			delete(r.gaps, r.nextSeq)
			r.nextSeq++
			continue
		}
		break
	}
	r.prune()
}

// Snapshot captures enough state to resume a repairer at a chunk boundary.
type Snapshot struct {
	Started   bool           `json:"started"`
	NextSeq   uint64         `json:"next_seq"`
	FloorSeq  uint64         `json:"floor_seq"`
	HaveTick  bool           `json:"have_tick"`
	LastRaw   uint64         `json:"last_raw"`
	LastTB    model.Timebase `json:"last_timebase"`
	Ticks     int64          `json:"ticks"`
	HaveMedia bool           `json:"have_media"`
	LastMedia int64          `json:"last_media"`
	LastDelta int64          `json:"last_delta"`
	Wraps     uint64         `json:"wraps"`
}

// Snapshot returns the current repairer state. It must only be taken when no
// frames are pending (the runner flushes before every checkpoint).
func (r *Repairer) Snapshot() Snapshot {
	return Snapshot{
		Started: r.started, NextSeq: r.nextSeq, FloorSeq: r.floorSeq,
		HaveTick: r.haveTick, LastRaw: r.lastRaw, LastTB: r.lastTB, Ticks: r.ticks,
		HaveMedia: r.haveMedia, LastMedia: r.lastMedia, LastDelta: r.lastDelta,
		Wraps: r.wraps,
	}
}

// Restore reinstates a snapshot produced at the same committed position.
func (r *Repairer) Restore(s Snapshot) {
	r.started = s.Started
	r.nextSeq = s.NextSeq
	r.floorSeq = s.FloorSeq
	r.haveTick = s.HaveTick
	r.lastRaw = s.LastRaw
	r.lastTB = s.LastTB
	r.ticks = s.Ticks
	r.haveMedia = s.HaveMedia
	r.lastMedia = s.LastMedia
	r.lastDelta = s.LastDelta
	r.wraps = s.Wraps
	r.pending = make(map[uint64]*model.Frame)
	r.gaps = make(map[uint64]struct{})
	r.recent = make(map[uint64]uint64)
}

// Push accepts one validated frame and returns all frames that became
// emit-able as a result, in increasing sequence order.
func (r *Repairer) Push(fr *model.Frame) []Event {
	if !fr.Timebase.Valid() {
		return []Event{quarantine(fr, model.CodeBadTimebase, fmt.Sprintf("invalid timebase %s", fr.Timebase), nil)}
	}
	if _, dup := r.recent[fr.Seq]; dup {
		return []Event{quarantine(fr, model.CodeDuplicateSeq, fmt.Sprintf("sequence %d was already indexed", fr.Seq), nil)}
	}
	r.recent[fr.Seq] = uint64(fr.Seq)
	if !r.started {
		// The first frame defines where the sequence numbering starts. Every
		// lower number is permanently absent, so a late arrival below
		// floorSeq is never reopened; nothing needs to be stored per gap.
		r.started = true
		r.nextSeq = fr.Seq
		r.floorSeq = fr.Seq
	}
	if fr.Seq < r.nextSeq {
		return []Event{quarantine(fr, model.CodeOutOfOrder,
			fmt.Sprintf("sequence %d arrived after %d; too late for the reorder window", fr.Seq, r.nextSeq-1), nil)}
	}
	// A sequence number below the stream's first frame can never be exported.
	if fr.Seq < r.floorSeq {
		return []Event{quarantine(fr, model.CodeOutOfOrder,
			fmt.Sprintf("sequence %d precedes the stream start %d", fr.Seq, r.floorSeq), nil)}
	}
	// A sequence number previously reported lost can no longer join the
	// timeline: it would land behind already emitted samples.
	if _, lost := r.gaps[fr.Seq]; lost {
		return []Event{quarantine(fr, model.CodeOutOfOrder,
			fmt.Sprintf("sequence %d was declared lost before it arrived", fr.Seq), nil)}
	}
	r.pending[fr.Seq] = fr

	var out []Event
	for {
		if fr, ok := r.pending[r.nextSeq]; ok {
			delete(r.pending, r.nextSeq)
			out = append(out, r.emit(fr))
			r.nextSeq++
			r.prune()
			continue
		}
		if _, gap := r.gaps[r.nextSeq]; gap {
			delete(r.gaps, r.nextSeq)
			r.nextSeq++
			r.prune()
			continue
		}
		if len(r.pending) > r.opts.ReorderWindow {
			out = append(out, r.emitOldest())
			r.prune()
			continue
		}
		break
	}
	return out
}

// PendingCount returns how many frames are buffered waiting for a gap to fill.
func (r *Repairer) PendingCount() int { return len(r.pending) }

// NextSeq returns the lowest sequence number the repairer has not yet decided.
// A frame below it can never be indexed now.
func (r *Repairer) NextSeq() uint64 { return r.nextSeq }

// PendingFloor returns the smallest source byte offset among buffered frames,
// or (0, false) when nothing is pending. A checkpoint must resume at or before
// this offset or the pending frames would be lost.
func (r *Repairer) PendingFloor() (int64, bool) {
	floor := int64(0)
	found := false
	for _, fr := range r.pending {
		if !found || fr.StartOffset < floor {
			floor = fr.StartOffset
			found = true
		}
	}
	return floor, found
}

// Flush emits everything still buffered, in sequence order.
func (r *Repairer) Flush() []Event {
	var out []Event
	for len(r.pending) > 0 {
		out = append(out, r.emitOldest())
		r.prune()
	}
	return out
}

func (r *Repairer) prune() {
	keepFrom := uint64(0)
	if r.nextSeq > uint64(r.opts.ReorderWindow) {
		keepFrom = r.nextSeq - uint64(r.opts.ReorderWindow)
	}
	for seq := range r.recent {
		if seq < keepFrom {
			delete(r.recent, seq)
		}
	}
	for seq := range r.gaps {
		if seq < keepFrom {
			delete(r.gaps, seq)
		}
	}
}

func (r *Repairer) emitOldest() Event {
	seqs := make([]uint64, 0, len(r.pending))
	for seq := range r.pending {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	seq := seqs[0]
	fr := r.pending[seq]
	delete(r.pending, seq)
	if seq > r.nextSeq {
		// Everything between nextSeq and seq is genuinely absent.
		for s := r.nextSeq; s < seq; s++ {
			r.gaps[s] = struct{}{}
		}
	}
	if seq >= r.nextSeq {
		r.nextSeq = seq + 1
	}
	return r.emit(fr)
}

// emit assigns a media time. The source clock is unwrapped incrementally so a
// wrap contributes the (small) forward step instead of a 2^32 tick jump.
func (r *Repairer) emit(fr *model.Frame) Event {
	ev := Event{Frame: fr}
	step := r.deltaStep()

	if fr.Timebase != r.lastTB {
		// A timebase change breaks tick continuity. Keep the output clock
		// running at the previous cadence and rebase the tick accumulator.
		r.lastTB = fr.Timebase
		r.lastRaw = fr.Timestamp
		r.haveTick = true
		r.ticks = 0
		if r.haveMedia {
			fr.MediaTime = r.lastMedia + step
			r.lastMedia = fr.MediaTime
			ev.Status = StatusIndexed
			ev.Media = fr.MediaTime
			ev.Adjusted = true
			ev.Reason = "timebase change rebased onto the running media clock"
			return ev
		}
	} else if r.haveTick {
		d := tickDelta(r.lastRaw, fr.Timestamp, r.opts.WrapModulus)
		switch {
		case d < 0:
			// Backwards movement of |d| ticks (never a wrap: wraps are
			// resolved to the short forward way round).
			if -d <= r.opts.DriftTolerance {
				d = step
				ev.Adjusted = true
				ev.Reason = "backwards drift clamped"
			} else {
				return quarantine(fr, model.CodeUnrepairableTime,
					fmt.Sprintf("timestamp moved backwards by %d ticks (tolerance %d)", -d, r.opts.DriftTolerance), nil)
			}
		case d == 0:
			d = 1
			ev.Adjusted = true
			ev.Reason = "duplicate timestamp advanced by one tick"
		case r.lastDelta > 0 && d > 100*r.lastDelta && d > int64(float64(step)*50):
			return quarantine(fr, model.CodeUnrepairableTime,
				fmt.Sprintf("timestamp jumped forward by %d ticks (expected about %d)", d, r.lastDelta), nil)
		}
		if fr.Timestamp < r.lastRaw {
			r.wraps++
			ev.Reason = joinReason(ev.Reason, "timestamp wrap repaired")
		}
		r.ticks += d
		r.lastRaw = fr.Timestamp
	} else {
		r.haveTick = true
		r.lastRaw = fr.Timestamp
		r.ticks = 0
	}
	media, err := fr.Timebase.Clock90(uint64(r.ticks))
	if err != nil {
		return quarantine(fr, model.CodeUnrepairableTime, err.Error(), err)
	}
	if r.haveMedia && media <= r.lastMedia {
		// Rounding to 90 kHz can collapse two adjacent ticks. Keep strict
		// monotonicity: the MP4 sample table requires positive deltas.
		media = r.lastMedia + 1
		ev.Adjusted = true
		ev.Reason = joinReason(ev.Reason, "media clock clamped by 90 kHz rounding")
	}
	if r.haveMedia {
		r.lastDelta = media - r.lastMedia
	}
	r.haveMedia = true
	r.lastMedia = media
	fr.MediaTime = media
	ev.Status = StatusIndexed
	ev.Media = media
	return ev
}

// tickDelta returns the forward tick distance from prev to cur, resolving
// values more than half a modulus apart as backward movement. A wrap therefore
// yields a small positive delta.
func tickDelta(prev, cur uint64, modulus uint64) int64 {
	if modulus == 0 {
		modulus = 1 << 32
	}
	if cur >= prev {
		d := cur - prev
		if d > modulus/2 {
			// Forward distance beyond half the clock is far more likely a
			// large backwards step across the modulus boundary.
			return int64(cur) - int64(prev)
		}
		return int64(d)
	}
	d := cur - prev + modulus // wraps modulo 2^64 to the short forward way
	if d > modulus/2 {
		return int64(cur) - int64(prev)
	}
	return int64(d)
}

func (r *Repairer) deltaStep() int64 {
	if r.lastDelta > 0 {
		return r.lastDelta
	}
	return 1
}

func quarantine(fr *model.Frame, code, reason string, err error) Event {
	return Event{
		Frame:  fr,
		Status: StatusQuarantined,
		Reason: reason,
		Fault: &model.Fault{
			Code:   code,
			Seq:    fr.Seq,
			HasSeq: true,
			Offset: fr.FrameOffset,
			Reason: reason,
			Err:    err,
		},
	}
}

func joinReason(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
