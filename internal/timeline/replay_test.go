package timeline

import (
	"strconv"
	"testing"
)

func TestWatermarkMonotoneAndLateDetection(t *testing.T) {
	w := NewWatermark(0)
	w.Observe(100)
	if w.Current() != 100 {
		t.Fatalf("watermark = %d", w.Current())
	}
	if cur, advanced := w.Observe(90); advanced || cur != 100 {
		t.Fatalf("watermark moved backwards: current=%d advanced=%v", cur, advanced)
	}
	if !w.IsLate(80) {
		t.Fatal("event at 80 should be late")
	}
}

func TestReplayBoundedLateEventDeterministically(t *testing.T) {
	r := NewReplayer(1000, 32)
	id := func(v int64) string { return "e" + strconv.FormatInt(v, 10) }
	for i, tms := range []int64{0, 100, 200} {
		r.Add(Event{ID: id(tms), TrackID: 1, Sequence: i, EventMS: tms})
	}
	before := r.Hash()
	out := r.Add(Event{ID: id(150), TrackID: 1, Sequence: 3, EventMS: 150})
	if !out.Late || !out.Replayed || out.Unfixable {
		t.Fatalf("unexpected outcome: %+v", out)
	}
	if r.ReplayCount != 1 || r.Hash() == before {
		t.Fatalf("replay count/hash wrong: count=%d changed=%v", r.ReplayCount, r.Hash() != before)
	}
	if !out.HashStable {
		t.Fatal("replay hash should be immediately stable")
	}

	second := NewReplayer(1000, 32)
	for i, evMS := range []int64{0, 100, 200, 150} {
		second.Add(Event{ID: id(evMS), TrackID: 1, Sequence: i, EventMS: evMS})
	}
	if r.Hash() != second.Hash() {
		t.Fatal("replay is not deterministic")
	}
}

func TestReplayOutsideWindowAndCommittedChunks(t *testing.T) {
	r := NewReplayer(50, 32)
	for _, tms := range []int64{0, 100, 200} {
		r.Add(Event{ID: "e", EventMS: tms})
	}
	if err := r.CommitChunk(0, "chunk-hash"); err != nil {
		t.Fatal(err)
	}
	out := r.Add(Event{ID: "too-late", EventMS: 0})
	if !out.Late || !out.Unfixable || out.Replayed {
		t.Fatalf("outside retention must be unfixable: %+v", out)
	}
	if r.UnfixableGaps != 1 {
		t.Fatalf("unfixable=%d", r.UnfixableGaps)
	}
	if err := r.CommitChunk(0, "different"); err == nil {
		t.Fatal("committed chunk hash must be immutable")
	}
}

func TestSchedulePreservesVariableIntervals(t *testing.T) {
	r := NewReplayer(1000, 32)
	for i, tms := range []int64{0, 33, 100, 133} {
		r.Add(Event{ID: "v", TrackID: 1, Sequence: i, EventMS: tms})
	}
	s := r.Schedule()
	if len(s) != 4 {
		t.Fatalf("schedule len=%d", len(s))
	}
	if s[2].IntervalMS != 67 || s[3].IntervalMS != 33 {
		t.Fatalf("VFR intervals = %d,%d want 67,33", s[2].IntervalMS, s[3].IntervalMS)
	}
}
