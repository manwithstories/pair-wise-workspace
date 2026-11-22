package timeline

import (
	"errors"
	"testing"

	"github.com/clearliu/fieldcut/internal/clock"
)

func evAt(seq int, start clock.Ticks) *Event {
	return &Event{TrackID: 0, Sequence: seq, Kind: KindFrame, Start: start}
}

func TestReplayInsideWindow(t *testing.T) {
	eng := NewReplayEngine(clock.Ticks(4 * clock.Rate))
	for s := 0; s < 10; s++ {
		eng.Append(evAt(s, clock.Ticks(s)*clock.Ticks(clock.Rate)))
	}
	// Head is at 9s; a late event at 7.5s sits inside the 4s window.
	late := evAt(99, clock.Ticks(7500*90))
	rep := eng.IngestLate(late)
	if rep.Outcome != ReplayAccepted {
		t.Fatalf("outcome = %v, want accepted", rep.Outcome)
	}
	if rep.RecomputedFrom != late.Start {
		t.Fatalf("recompute from = %d, want %d", rep.RecomputedFrom, late.Start)
	}
	r, i := eng.Stats()
	if r != 1 || i != 0 {
		t.Fatalf("stats replays=%d isolated=%d", r, i)
	}
	committed := eng.Committed()
	// The late event must be spliced at its sorted position, not at tail.
	var foundAt int
	for idx, ev := range committed {
		if ev.Sequence == 99 {
			foundAt = idx
		}
	}
	if foundAt != 8 {
		t.Fatalf("late event spliced at %d, want 8", foundAt)
	}
	// Deterministic tail recomputation.
	tail1 := eng.Recompute(late.Start)
	tail2 := eng.Recompute(late.Start)
	if len(tail1) != len(tail2) {
		t.Fatal("recompute length differs across runs")
	}
	for i := range tail1 {
		if tail1[i].Sequence != tail2[i].Sequence || tail1[i].Start != tail2[i].Start {
			t.Fatal("recompute non-deterministic")
		}
	}
}

func TestReplayOutsideWindowIsolated(t *testing.T) {
	eng := NewReplayEngine(clock.Ticks(2 * clock.Rate))
	for s := 0; s < 6; s++ {
		eng.Append(evAt(s, clock.Ticks(s)*clock.Ticks(clock.Rate)))
	}
	before := len(eng.Committed())
	// Head at 5s; event at 0.5s is 4.5s behind > 2s window.
	late := evAt(77, clock.Ticks(500*90))
	rep := eng.IngestLate(late)
	if rep.Outcome != ReplayIsolated {
		t.Fatalf("outcome = %v, want isolated", rep.Outcome)
	}
	if len(eng.Committed()) != before {
		t.Fatal("isolated event must not mutate committed timeline")
	}
	q := eng.Quarantine()
	if len(q) != 1 || q[0].Event.Sequence != 77 {
		t.Fatalf("quarantine wrong: %+v", q)
	}
	r, i := eng.Stats()
	if r != 0 || i != 1 {
		t.Fatalf("stats replays=%d isolated=%d", r, i)
	}
}

func TestReplayWindowHardCap(t *testing.T) {
	eng := NewReplayEngine(clock.Ticks(clock.MaxLateWindowTicks + clock.Rate))
	cp := eng.Checkpoint()
	if cp.Window != clock.Ticks(clock.MaxLateWindowTicks) {
		t.Fatalf("window = %d, want hard cap %d", cp.Window, clock.MaxLateWindowTicks)
	}
}

func TestReplayCheckpointResume(t *testing.T) {
	eng := NewReplayEngine(clock.Ticks(4 * clock.Rate))
	for s := 0; s < 5; s++ {
		eng.Append(evAt(s, clock.Ticks(s)*clock.Ticks(clock.Rate)))
	}
	cp := eng.Checkpoint()
	if cp.EventCount != 5 || cp.CommittedThrough != clock.Ticks(4*clock.Rate) {
		t.Fatalf("checkpoint wrong: %+v", cp)
	}
	if err := eng.Resume(cp); err != nil {
		t.Fatalf("valid checkpoint rejected: %v", err)
	}
	bad := cp
	bad.EventCount = 99
	if err := eng.Resume(bad); !errors.Is(err, clock.ErrCheckpoint) {
		t.Fatalf("future checkpoint: want ErrCheckpoint, got %v", err)
	}
	bad = cp
	bad.Window += clock.Ticks(clock.Rate)
	if err := eng.Resume(bad); !errors.Is(err, clock.ErrCheckpoint) {
		t.Fatalf("window mismatch: want ErrCheckpoint, got %v", err)
	}
	bad = cp
	bad.CommittedThrough += clock.Ticks(clock.Rate)
	if err := eng.Resume(bad); !errors.Is(err, clock.ErrCheckpoint) {
		t.Fatalf("head mismatch: want ErrCheckpoint, got %v", err)
	}
}

func TestReplayDeterministicAcrossRuns(t *testing.T) {
	run := func() []clock.Ticks {
		eng := NewReplayEngine(clock.Ticks(4 * clock.Rate))
		for s := 0; s < 8; s++ {
			eng.Append(evAt(s, clock.Ticks(s)*clock.Ticks(clock.Rate)))
		}
		// Two late events arrive in reverse order; sorted splice must
		// produce the same timeline regardless of arrival sequence.
		eng.IngestLate(evAt(101, clock.Ticks(2500*90)))
		eng.IngestLate(evAt(100, clock.Ticks(1500*90)))
		var starts []clock.Ticks
		for _, ev := range eng.Committed() {
			starts = append(starts, ev.Start)
		}
		return starts
	}
	a, b := run(), run()
	if len(a) != len(b) {
		t.Fatalf("lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("non-deterministic at %d: %d vs %d", i, a[i], b[i])
		}
	}
}
