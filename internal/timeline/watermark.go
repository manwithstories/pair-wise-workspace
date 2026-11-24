package timeline

import (
	"fmt"

	"github.com/clearliu/mediq/internal/core"
)

// Event is one contiguous media span on the unified clock.
type Event struct {
	Sequence int64
	Start    core.Time
	End      core.Time
}

// Range is a committed unified-time interval.
type Range struct {
	Index int64
	Start core.Time
	End   core.Time
}

// Length returns the interval length in 48 kHz samples.
func (r Range) Length() int64 { return r.End.Samples() - r.Start.Samples() }

// Checkpoint is a deterministic rollback point for one committed interval.
type Checkpoint struct {
	Interval     int64
	Start        core.Time
	End          core.Time
	Watermark    core.Time
	CommittedEnd core.Time
	Token        string
}

// Config bounds watermark lateness and checkpoint retention.
type Config struct {
	IntervalSamples    int64
	LatenessSamples    int64
	RetentionIntervals int64
}

// Watermark advances event time and retains bounded rollback checkpoints.
type Watermark struct {
	cfg          Config
	maxEnd       core.Time
	watermark    core.Time
	committedEnd core.Time
	checkpoints  []Checkpoint
	LateReplays  int
	LateExpired  int
}

// New creates a watermark. Zero config values receive bounded defaults.
func New(cfg Config) *Watermark {
	if cfg.IntervalSamples <= 0 {
		cfg.IntervalSamples = core.OutputRateHz / 10 // 100 ms
	}
	if cfg.RetentionIntervals <= 0 {
		cfg.RetentionIntervals = 30 // 3 s at the default interval
	}
	if cfg.RetentionIntervals > 1000 {
		cfg.RetentionIntervals = 1000
	}
	return &Watermark{cfg: cfg}
}

// CommittedEnd returns the current committed position.
func (w *Watermark) CommittedEnd() core.Time { return w.committedEnd }

// Current returns the current event-time watermark.
func (w *Watermark) Current() core.Time { return w.watermark }

// Checkpoints returns retained checkpoints in interval order.
func (w *Watermark) Checkpoints() []Checkpoint {
	out := make([]Checkpoint, len(w.checkpoints))
	copy(out, w.checkpoints)
	return out
}

// Observe processes an event and returns newly committed ranges.
//
// If a late event is inside retention, Replay identifies the checkpoint to
// restore and Ranges lists the suffix that must be deterministically
// recomputed. If it is outside retention, Rejected is true and state is left
// unchanged.
func (w *Watermark) Observe(ev Event) (ranges []Range, replay *Checkpoint, rejected bool) {
	if ev.End > w.maxEnd {
		w.maxEnd = ev.End
	}
	if ev.End <= w.committedEnd {
		cp := w.findCheckpoint(ev.Start)
		if cp == nil {
			w.LateExpired++
			return nil, nil, true
		}
		w.rewind(*cp)
		w.LateReplays++
		replay = cp
	}
	ranges = w.advance(false)
	return ranges, replay, false
}

// Flush commits the final partial range at end of stream.
func (w *Watermark) Flush() []Range {
	return w.advance(true)
}

func (w *Watermark) findCheckpoint(t core.Time) *Checkpoint {
	idx := int64(t) / w.cfg.IntervalSamples
	for i := range w.checkpoints {
		if w.checkpoints[i].Interval == idx {
			cp := w.checkpoints[i]
			return &cp
		}
	}
	return nil
}

func (w *Watermark) rewind(cp Checkpoint) {
	w.committedEnd = cp.Start
	kept := w.checkpoints[:0]
	for _, existing := range w.checkpoints {
		if existing.Interval < cp.Interval {
			kept = append(kept, existing)
		}
	}
	w.checkpoints = kept
}

func (w *Watermark) advance(final bool) []Range {
	var newly []Range
	wm := w.maxEnd - core.Time(w.cfg.LatenessSamples)
	if wm < 0 {
		wm = 0
	}
	interval := w.cfg.IntervalSamples
	committedInterval := int64(w.committedEnd) / interval

	for {
		start := core.Time(committedInterval * interval)
		end := start + core.Time(interval)
		if end > wm && !(final && w.maxEnd > start) {
			break
		}
		if final && end > w.maxEnd {
			end = w.maxEnd
		}
		if end <= start {
			break
		}

		r := Range{Index: committedInterval, Start: start, End: end}
		w.committedEnd = end
		w.watermark = end
		cp := Checkpoint{
			Interval:     committedInterval,
			Start:        start,
			End:          end,
			Watermark:    wm,
			CommittedEnd: end,
			Token:        fmt.Sprintf("deterministic-cp-%012d", committedInterval),
		}
		w.checkpoints = append(w.checkpoints, cp)
		newly = append(newly, r)
		committedInterval++

		// A partial final range terminates advancement.
		if end%core.Time(interval) != 0 {
			break
		}
	}
	w.prune()
	return newly
}

func (w *Watermark) prune() {
	if len(w.checkpoints) == 0 {
		return
	}
	last := w.checkpoints[len(w.checkpoints)-1].Interval
	minKept := last - w.cfg.RetentionIntervals + 1
	first := 0
	for first < len(w.checkpoints) && w.checkpoints[first].Interval < minKept {
		first++
	}
	if first > 0 {
		w.checkpoints = append([]Checkpoint(nil), w.checkpoints[first:]...)
	}
}
