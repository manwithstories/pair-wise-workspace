// Package timeline converts per-frame timestamps to the unified 90 kHz media
// clock and repairs wrapping, duplicate, out-of-order and drifting input.
package timeline

import (
	"fmt"
	"sort"

	"mediq/internal/model"
)

// Status is the outcome assigned to a frame by the repairer.
type Status int

const (
	StatusIndexed Status = iota
	StatusQuarantined
)

// Event describes a repair decision for one frame.
type Event struct {
	Frame    *model.Frame
	Status   Status
	Reason   string
	Fault    *model.Fault
	Media    int64
	Adjusted bool
}

// Options tunes repair heuristics. All tolerances are in 90 kHz ticks.
type Options struct {
	// ReorderWindow bounds how many sequence gaps are buffered before the
	// repairer makes progress anyway. Default 32.
	ReorderWindow int
	// DriftTolerance is the largest backward timestamp movement that is
	// treated as jitter and clamped forward. Default 450 (5 ms).
	DriftTolerance int64
	// WrapModulus is the modulus of the source timestamp clock. Default 1<<32.
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

// Repairer reorders a bounded window of frames by sequence number and assigns
// each emitted frame an absolute 90 kHz media time.
type Repairer struct {
	opts Options

	started  bool
	nextSeq  uint64
	pending  map[uint64]*model.Frame
	seen     map[uint64]struct{}
	lastRaw  uint64
	lastTB   model.Timebase
	haveRaw  bool
	wraps    uint64

	haveMedia bool
	lastMedia int64
	lastDelta int64
}

func New(opts Options) *Repairer {
	return &Repairer{
		opts:    opts.withDefaults(),
		pending: make(map[uint64]*model.Frame),
		seen:    make(map[uint64]struct{}),
	}
}

// Wraps reports how many timestamp wraps were repaired.
func (r *Repairer) Wraps() uint64 { return r.wraps }

// Push accepts a frame and returns every frame that became emit-able.
func (r *Repairer) Push(fr *model.Frame) []Event {
	if !fr.Timebase.Valid() {
		return []Event{quarantine(fr, model.CodeBadTimebase, fmt.Sprintf("invalid timebase %s", fr.Timebase), nil)}
	}
	if _, dup := r.seen[fr.Seq]; dup {
		return []Event{quarantine(fr, model.CodeDuplicateSeq, fmt.Sprintf("sequence %d already indexed", fr.Seq), nil)}
	}
	if int64(fr.Seq) < 0 {
		return []Event{quarantine(fr, model.CodeOutOfOrder, "sequence number out of range", nil)}
	}
	r.seen[fr.Seq] = struct{}{}
	if !r.started {
		r.started = true
		r.nextSeq = fr.Seq
	}
	if fr.Seq < r.nextSeq {
		return []Event{quarantine(fr, model.CodeOutOfOrder, fmt.Sprintf("sequence %d arrived after %d", fr.Seq, r.nextSeq-1), nil)}
	}
	r.pending[fr.Seq] = fr

	var out []Event
	for {
		next, ok := r.pending[r.nextSeq]
		if ok {
			delete(r.pending, r.nextSeq)
			out = append(out, r.emit(next))
			r.nextSeq++
			continue
		}
		// A gap: keep buffering, but never let memory grow past the window.
		if len(r.pending) > r.opts.ReorderWindow {
			out = append(out, r.emitOldest())
			continue
		}
		break
	}
	return out
}

// Flush emits every buffered frame in sequence order.
func (r *Repairer) Flush() []Event {
	var out []Event
	for len(r.pending) > 0 {
		out = append(out, r.emitOldest())
	}
	return out
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
	if seq >= r.nextSeq {
		r.nextSeq = seq + 1
	}
	return r.emit(fr)
}

func (r *Repairer) emit(fr *model.Frame) Event {
	unwrapped, wrapped := r.unwrap(fr)
	media, err := fr.Timebase.Clock90(unwrapped)
	if err != nil {
		return quarantine(fr, model.CodeUnrepairableTime, err.Error(), err)
	}
	ev := Event{Frame: fr}
	if wrapped {
		r.wraps++
	}

	if !r.haveMedia {
		r.haveMedia = true
		r.lastMedia = media
		fr.MediaTime = media
		ev.Status = StatusIndexed
		ev.Media = media
		return ev
	}

	delta := media - r.lastMedia
	expected := r.lastMedia + r.deltaStep()
	switch {
	case delta <= 0:
		if -delta <= r.opts.DriftTolerance {
			// Jitter or rounding moved the timestamp backwards: clamp it
			// forward so the media timeline stays strictly monotonic.
			media = expected
			if media <= r.lastMedia {
				media = r.lastMedia + 1
			}
			ev.Adjusted = true
			if wrapped {
				ev.Reason = "timestamp wrap clamped"
			} else {
				ev.Reason = "backwards jitter clamped"
			}
			ev.Status = StatusIndexed
			ev.Media = media
			fr.MediaTime = media
			break
		}
		return quarantine(fr, model.CodeUnrepairableTime,
			fmt.Sprintf("timestamp moved backwards by %d ticks (tolerance %d)", -delta, r.opts.DriftTolerance), nil)
	case r.lastDelta > 0 && abs64(delta-r.lastDelta) > 10*r.lastDelta && delta > 100*r.deltaStep():
		return quarantine(fr, model.CodeUnrepairableTime,
			fmt.Sprintf("timestamp jumped forward by %d ticks (expected about %d)", delta, r.lastDelta), nil)
	default:
		ev.Status = StatusIndexed
		ev.Media = media
		fr.MediaTime = media
	}
	r.lastMedia = media
	if delta > 0 {
		r.lastDelta = delta
	}
	return ev
}

func (r *Repairer) deltaStep() int64 {
	if r.lastDelta > 0 {
		return r.lastDelta
	}
	return 1
}

// unwrap converts the frame's timestamp into a monotonically increasing tick
// count, accounting for the source clock's modulus.
func (r *Repairer) unwrap(fr *model.Frame) (uint64, bool) {
	if !r.haveRaw || fr.Timebase != r.lastTB {
		r.haveRaw = true
		r.lastTB = fr.Timebase
		r.lastRaw = fr.Timestamp
		return fr.Timestamp, false
	}
	cur := fr.Timestamp
	half := r.opts.WrapModulus / 2
	wrapped := false
	if r.lastRaw > half && cur < r.lastRaw-half {
		cur += r.opts.WrapModulus
		wrapped = true
	} else if cur < r.lastRaw && r.lastRaw-cur > half {
		cur += r.opts.WrapModulus
		wrapped = true
	}
	if cur < r.lastRaw {
		cur = r.lastRaw // never hand back a pre-wrap value
	}
	r.lastRaw = cur
	return cur, wrapped
}

func quarantine(fr *model.Frame, code, reason string, err error) Event {
	fault := &model.Fault{
		Code:     code,
		Seq:      fr.Seq,
		HasSeq:   true,
		Offset:   fr.FrameOffset,
		Reason:   reason,
		Err:      err,
	}
	return Event{Frame: fr, Status: StatusQuarantined, Reason: reason, Fault: fault}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
