package timeline

import (
	"sort"

	"archiver/internal/media"
)

// Sink receives committed events in stable order. Snapshots must
// capture enough state for Restore to roll the sink back.
type Sink interface {
	Emit(ev Event)
	Snapshot() []byte
	Restore(snap []byte)
}

// Checkpoint records the committed watermark, emitted log length and
// sink/clock state so processing can resume deterministically.
type Checkpoint struct {
	LogLen    int
	Watermark media.TimeUs
	Sink      []byte
	Clock     []byte
}

// Replayer commits events in stable (time, SSRC, ext seq) order as the
// watermark advances. Late events inside the rollback horizon trigger
// a restart from the nearest checkpoint; events beyond the horizon are
// quarantined. Memory is bounded by the horizon and the pending set.
type Replayer struct {
	wm        *Watermark
	horizon   media.TimeUs
	cpEvery   int
	sink      Sink
	clockSnap func() []byte
	clockLoad func([]byte)

	pending []Event
	log     []Event
	cps     []Checkpoint
	drained media.TimeUs
	sinceCp int

	Replays     int
	Quarantined int
}

func NewReplayer(wm *Watermark, horizon media.TimeUs, cpEvery int, sink Sink,
	clockSnap func() []byte, clockLoad func([]byte)) *Replayer {
	if cpEvery <= 0 {
		cpEvery = 64
	}
	r := &Replayer{
		wm: wm, horizon: horizon, cpEvery: cpEvery, sink: sink,
		clockSnap: clockSnap, clockLoad: clockLoad,
	}
	// Checkpoint zero: the pristine state everything can roll back to.
	r.cps = append(r.cps, Checkpoint{Watermark: 0, Sink: sink.Snapshot(), Clock: clockSnap()})
	return r
}

// Add ingests one event, rolling back and recomputing when it is late
// but still inside the horizon.
func (r *Replayer) Add(ev Event) {
	r.wm.Observe(ev.Track, ev.TimeUs)
	if len(r.log) > 0 && ev.TimeUs <= r.drained {
		if r.drained-ev.TimeUs > r.horizon {
			r.Quarantined++
			return
		}
		r.rollback(ev.TimeUs)
	}
	r.pending = insertSorted(r.pending, ev)
	r.drain()
}

func insertSorted(s []Event, ev Event) []Event {
	i := sort.Search(len(s), func(i int) bool { return !lessEq(s[i], ev) })
	return append(s[:i], append([]Event{ev}, s[i:]...)...)
}

// lessEq orders by (time, SSRC, ext seq); stable for equal keys.
func lessEq(a, b Event) bool {
	if a.TimeUs != b.TimeUs {
		return a.TimeUs < b.TimeUs
	}
	if a.Track.SSRC != b.Track.SSRC {
		return a.Track.SSRC < b.Track.SSRC
	}
	return a.ExtSeq <= b.ExtSeq
}

// rollback restores the nearest checkpoint at or before t and returns
// the events emitted after it to the pending queue for recomputation.
func (r *Replayer) rollback(t media.TimeUs) {
	idx := 0
	for i, cp := range r.cps {
		if cp.Watermark <= t {
			idx = i
		}
	}
	cp := r.cps[idx]
	r.sink.Restore(cp.Sink)
	r.clockLoad(cp.Clock)
	r.pending = append(r.log[cp.LogLen:], r.pending...)
	r.log = r.log[:cp.LogLen]
	r.cps = r.cps[:idx+1]
	r.drained = cp.Watermark
	r.sinceCp = 0
	r.Replays++
}

// drain emits every pending event at or below the watermark.
func (r *Replayer) drain() {
	wm, ok := r.wm.Current()
	if !ok {
		return
	}
	n := 0
	for n < len(r.pending) && r.pending[n].TimeUs <= wm {
		n++
	}
	for _, ev := range r.pending[:n] {
		r.sink.Emit(ev)
		r.log = append(r.log, ev)
		r.sinceCp++
		if r.sinceCp >= r.cpEvery {
			r.checkpoint(wm)
		}
	}
	r.pending = append([]Event{}, r.pending[n:]...)
	if n > 0 {
		r.drained = wm
		r.prune()
	}
}

func (r *Replayer) checkpoint(wm media.TimeUs) {
	r.cps = append(r.cps, Checkpoint{
		LogLen:    len(r.log),
		Watermark: wm,
		Sink:      r.sink.Snapshot(),
		Clock:     r.clockSnap(),
	})
	r.sinceCp = 0
}

// prune bounds memory: checkpoints older than the horizon are dropped
// and the log prefix they referenced is released.
func (r *Replayer) prune() {
	cut := r.drained - r.horizon
	keep := 0
	for keep < len(r.cps)-1 && r.cps[keep+1].Watermark <= cut {
		keep++
	}
	if keep == 0 {
		return
	}
	base := r.cps[keep].LogLen
	r.cps = append([]Checkpoint{}, r.cps[keep:]...)
	r.log = append([]Event{}, r.log[base:]...)
	for i := range r.cps {
		r.cps[i].LogLen -= base
	}
}

// Flush emits everything still pending (end of stream).
func (r *Replayer) Flush() {
	for _, ev := range r.pending {
		r.sink.Emit(ev)
		r.log = append(r.log, ev)
	}
	r.pending = nil
}

// Stats reports rollback and quarantine counters.
func (r *Replayer) Stats() (replays, quarantined int) { return r.Replays, r.Quarantined }
