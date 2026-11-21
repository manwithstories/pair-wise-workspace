package timeline

import (
	"fmt"

	"mediameld/internal/model"
)

// Replayer advances a watermark over an arrival-ordered event stream. When an
// event arrives before the watermark minus the late window, it rewinds to the
// nearest checkpoint and replays the affected suffix deterministically.
type Replayer struct {
	cfg        Config
	emitted    []model.Event
	checkpoint map[int][]model.Event // segment index -> emitted prefix
	seen       map[string]bool
	replays    int
	maxPTS     model.Instant
}

// NewReplayer prepares a replayer with the given late window and segment span.
func NewReplayer(cfg Config) (*Replayer, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Replayer{
		cfg:        cfg,
		checkpoint: map[int][]model.Event{},
		seen:       map[string]bool{},
	}, nil
}

func eventKey(e model.Event) string {
	return fmt.Sprintf("%s#%d#%d", e.TrackID, e.Seq, e.Index)
}

// Reachable reports the current watermark: max observed PTS minus the allowed
// late window. Until any event is seen it is zero.
func (r *Replayer) Watermark() model.Instant {
	w := r.maxPTS - r.cfg.AllowedLate
	if w < 0 {
		return 0
	}
	return w
}

// Push consumes one arrival-ordered event. It returns the events newly
// confirmed up to the (possibly retreated) watermark boundary.
func (r *Replayer) Push(e model.Event) []model.Event {
	key := eventKey(e)
	if r.seen[key] {
		// Duplicate packet: never re-enter a sample.
		return nil
	}
	r.seen[key] = true

	late := e.PTS < r.Watermark()
	if e.PTS > r.maxPTS {
		r.maxPTS = e.PTS
	}
	if late {
		r.replays++
		r.rewind(e)
	}
	r.emitted = append(r.emitted, e)
	sortEvents(r.emitted)
	r.saveCheckpoint()
	return nil
}

// rewind discards emitted events at or after the checkpoint segment so the
// replay recomputes them from a stable prefix.
func (r *Replayer) rewind(e model.Event) {
	cp := CheckpointIndex(e.PTS, r.cfg.SegmentSpan)
	if prefix, ok := r.checkpoint[cp]; ok {
		r.emitted = append([]model.Event(nil), prefix...)
		// Forget confirmations for the discarded suffix so a resend is not
		// treated as a duplicate.
		for _, ev := range prefix {
			r.seen[eventKey(ev)] = true
		}
	}
}

func (r *Replayer) saveCheckpoint() {
	if len(r.emitted) == 0 {
		return
	}
	seg := SegmentFor(r.emitted[len(r.emitted)-1].PTS, r.cfg.SegmentSpan)
	r.checkpoint[seg] = append([]model.Event(nil), r.emitted...)
}

// Events returns the ordered timeline accumulated so far.
func (r *Replayer) Events() []model.Event {
	return append([]model.Event(nil), r.emitted...)
}

// Replays reports how many late arrivals forced a rewind.
func (r *Replayer) Replays() int { return r.replays }

// Finalize assigns segments and computes the deterministic hash, mirroring
// Build's output shape so streaming and batch paths agree.
func (r *Replayer) Finalize() Plan {
	events := r.Events()
	sortEvents(events)
	plan := Plan{
		Events:      events,
		Segments:    segmentEvents(events, r.cfg.SegmentSpan),
		LateReplays: r.replays,
		Gaps:        countGaps(events),
		Watermark:   r.Watermark(),
	}
	plan.Hash = HashEvents(events)
	return plan
}
