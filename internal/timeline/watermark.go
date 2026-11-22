// Package timeline turns validated ingest events into a single ordered,
// watermark-committed media timeline. It repairs per-track timestamp
// defects (regression, duplication, 32/33-bit wrap, linear node drift),
// marks ranges that cannot be uniquely inferred as untrusted, and hands
// late events to the deterministic replay engine.
package timeline

import (
	"fmt"
	"sort"

	"github.com/clearliu/fieldcut/internal/clock"
	"github.com/clearliu/fieldcut/internal/ingest"
)

// EventKind distinguishes real media from structural placeholders.
type EventKind int

const (
	KindFrame EventKind = iota
	KindGap // an unrecoverable packet hole; never carries a fabricated frame
)

// Event is one timeline element. Ticks are always on the unified 90 kHz
// axis after TrackRepair has touched them.
type Event struct {
	TrackID   int
	Sequence  int
	Kind      EventKind
	RawTS     uint32 // wire timestamp before repair (RTP 32-bit)
	Start     clock.Ticks
	End       clock.Ticks
	Source    ingest.Source
	Recovered bool
	Untrusted bool
	// Defect summarizes why repair was needed; empty for clean events.
	Defect string
}

// RepairConfig parameterizes per-track timestamp repair.
type RepairConfig struct {
	// WrapBits selects 32 (RTP) or 33 (MPEG-TS) unwrapping.
	WrapBits int
	// MaxRegressionTicks: a backward jump no larger than this is treated
	// as a duplicate/rewind and repaired; larger regressions are marked
	// untrusted because no unique mapping exists.
	MaxRegressionTicks clock.Ticks
	// DriftPPM bounds the accepted node oscillator slope.
	DriftPPM int64
}

// DefaultRepairConfig returns conservative production defaults.
func DefaultRepairConfig() RepairConfig {
	return RepairConfig{
		WrapBits:           32,
		MaxRegressionTicks: clock.Ticks(clock.Rate), // 1 s
		DriftPPM:           10_000,                 // 1%
	}
}

// TrackRepair normalizes one track's raw timestamps. All math is pure so
// identical input sequences produce identical repaired output.
type TrackRepair struct {
	cfg        RepairConfig
	drift      *clock.DriftEstimator
	slope      float64
	intercept  clock.Ticks
	haveFit    bool
	lastRaw    clock.Ticks // last unwrapped raw value
	lastFixed  clock.Ticks // last corrected output
	started    bool
	nOut       int
	seenExt    map[clock.Ticks]bool
}

// NewTrackRepair constructs a repairer for a single track.
func NewTrackRepair(cfg RepairConfig) *TrackRepair {
	if cfg.WrapBits != 32 && cfg.WrapBits != 33 {
		cfg.WrapBits = 32
	}
	if cfg.MaxRegressionTicks <= 0 {
		cfg.MaxRegressionTicks = clock.Ticks(clock.Rate)
	}
	return &TrackRepair{
		cfg:     cfg,
		drift:   clock.NewDriftEstimator(cfg.DriftPPM),
		slope:   1,
		seenExt: map[clock.Ticks]bool{},
	}
}

// Repair maps one wire timestamp onto the corrected axis. The returned
// event copy has Start/Untrusted/Defect populated; callers fill the rest.
func (r *TrackRepair) Repair(raw uint32) (fixed clock.Ticks, untrusted bool, defect string, err error) {
	var unwrapped clock.Ticks
	if !r.started {
		// First sample establishes the axis at its absolute wire value;
		// taking a delta against the zero tick would fabricate a wrap.
		unwrapped = clock.Ticks(raw)
	} else if r.cfg.WrapBits == 33 {
		unwrapped = clock.Unwrap33(r.lastRaw, uint64(raw))
	} else {
		unwrapped = clock.Unwrap32(r.lastRaw, raw)
	}
	r.lastRaw = unwrapped
	duplicate := r.started && r.seenExt[unwrapped]
	if duplicate {
		defect = "duplicate"
	}
	r.seenExt[unwrapped] = true

	// Drift correction uses the fitted affine model once enough anchors
	// exist; before that it is identity on the unwrapped axis.
	if r.haveFit {
		fixed = clock.Correct(unwrapped, r.slope, r.intercept)
	} else {
		fixed = unwrapped
	}

	// A repeated timestamp repeats the last position by definition; only
	// genuinely new values need monotonicity enforcement.
	if r.nOut > 0 && !duplicate {
		switch {
		case fixed < r.lastFixed && r.lastFixed-fixed <= r.cfg.MaxRegressionTicks:
			defect = joinDefect(defect, "regression")
			fixed = r.lastFixed // clamp to last known good position
		case fixed < r.lastFixed:
			defect = joinDefect(defect, "unresolvable-regression")
			untrusted = true
		}
	}
	r.lastFixed = fixed
	r.nOut++
	r.started = true
	return fixed, untrusted, defect, nil
}

// Anchor feeds a (local, reference) correspondence and refits the drift
// model. A slope beyond the PPM envelope marks the whole track untrusted
// from this point instead of applying an implausible correction.
func (r *TrackRepair) Anchor(local, ref clock.Ticks) error {
	r.drift.Anchor(local, ref)
	if r.drift.N() < 2 {
		r.intercept = ref - local
		return nil
	}
	slope, ic, err := r.drift.Fit()
	if err != nil {
		return err
	}
	r.slope, r.intercept, r.haveFit = slope, ic, true
	return nil
}

func joinDefect(a, b string) string {
	if a == "" {
		return b
	}
	return a + "+" + b
}

// Watermark commits events by event time. Events are held until every
// active track has advanced past their Start: the commit point is the
// minimum of the per-track high-water marks. Released events come back in
// a total (Start, TrackID, Sequence) order, which makes replay
// deterministic.
type Watermark struct {
	repairs map[int]*TrackRepair
	cfg     RepairConfig
	// pending holds not-yet-releasable events keyed by track, in arrival
	// order. Per-track queues stay monotonic after repair.
	pending map[int][]*Event
	// high is the highest observed Start per track.
	high map[int]clock.Ticks
}

// NewWatermark builds a watermark with one repairer per track, created
// lazily so callers do not need to know track IDs up front.
func NewWatermark(cfg RepairConfig) *Watermark {
	return &Watermark{
		repairs: map[int]*TrackRepair{},
		cfg:     cfg,
		pending: map[int][]*Event{},
		high:    map[int]clock.Ticks{},
	}
}

// commitMark returns the global event-time point up to which data is
// safe. A track's first event is buffered until that track has
// established a high-water mark, so a newly appearing slow track cannot
// be drained before it participates in the mark; once all active tracks
// have reported, the slowest one sets the point.
func (w *Watermark) commitMark() clock.Ticks {
	var mark clock.Ticks
	first := true
	for track := range w.pending {
		h, ok := w.high[track]
		if !ok || len(w.pending[track]) == 0 {
			// Track still holds its first event (or nothing at all):
			// nothing beyond the current event's own position can be
			// globally safe yet. Returning 0 drains only t=0 events.
			return 0
		}
		if first || h < mark {
			mark, first = h, false
		}
	}
	return mark
}

// Commit ingests one event, repairs its timestamp and returns everything
// that the new cross-track watermark makes safe, in total order. Gap
// events pass through without timestamp repair.
func (w *Watermark) Commit(ev *Event) ([]*Event, error) {
	if ev == nil {
		return nil, fmt.Errorf("%w: nil event", clock.ErrInvalidArgument)
	}
	if ev.Kind == KindGap {
		ev.Untrusted = true
		w.pending[ev.TrackID] = append(w.pending[ev.TrackID], ev)
	} else {
		rep := w.repairs[ev.TrackID]
		if rep == nil {
			rep = NewTrackRepair(w.cfg)
			w.repairs[ev.TrackID] = rep
		}
		fixed, untrusted, defect, err := rep.Repair(ev.RawTS)
		if err != nil {
			return nil, err
		}
		ev.Start = fixed
		ev.Untrusted = ev.Untrusted || untrusted
		if defect != "" && ev.Defect == "" {
			ev.Defect = defect
		}
		w.pending[ev.TrackID] = append(w.pending[ev.TrackID], ev)
	}
	if ev.Start > w.high[ev.TrackID] {
		w.high[ev.TrackID] = ev.Start
	}
	// Every event is enqueued; only the prefix at or below the
	// cross-track mark is released, so a slow consumer track holds back
	// the fast track's future frames without losing them.
	return w.drain(w.commitMark()), nil
}

// drain releases the per-track prefixes at or below the commit mark.
func (w *Watermark) drain(mark clock.Ticks) []*Event {
	var ready []*Event
	for track, queue := range w.pending {
		keep := 0
		for keep < len(queue) && queue[keep].Start <= mark {
			ready = append(ready, queue[keep])
			keep++
		}
		w.pending[track] = append(queue[:0], queue[keep:]...)
	}
	sortEvents(ready)
	return ready
}

// Flush releases every held event at stream end.
func (w *Watermark) Flush() []*Event {
	var ready []*Event
	for _, queue := range w.pending {
		ready = append(ready, queue...)
	}
	w.pending = map[int][]*Event{}
	sortEvents(ready)
	return ready
}

// sortEvents imposes the project's total deterministic event order.
func sortEvents(evs []*Event) {
	sort.SliceStable(evs, func(i, j int) bool {
		if evs[i].Start != evs[j].Start {
			return evs[i].Start < evs[j].Start
		}
		if evs[i].TrackID != evs[j].TrackID {
			return evs[i].TrackID < evs[j].TrackID
		}
		return evs[i].Sequence < evs[j].Sequence
	})
}
