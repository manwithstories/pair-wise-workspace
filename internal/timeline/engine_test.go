package timeline

import (
	"testing"

	"mediapack/internal/clock"
)

func newTestEngine() *Engine {
	return NewEngine(Config{
		Timebase: clock.DefaultTimebase(),
		LeadNS:   500_000_000, // 500ms watermark lag
		WindowNS: 2_000_000_000,
	})
}

func ev(seq int64, track uint64, timeMS int64, off int64) Event {
	return Event{
		Seq: seq, Track: track,
		TimeNS: timeMS * 1_000_000,
		RawTime: timeMS, ByteOff: off, PayloadLen: 10,
		Keyframe: track == 1,
	}
}

func TestWatermarkCommit(t *testing.T) {
	e := newTestEngine()
	// First event establishes the time base but cannot cross a watermark.
	r1 := e.Push(ev(1, 1, 0, 100))
	if len(r1.Committed) != 0 {
		t.Fatalf("first event should stay pending, got %d committed", len(r1.Committed))
	}
	// Event at 1000ms: new watermark = 1000-500 = 500ms. The 0ms event
	// is strictly below 500ms and commits; the 1000ms event stays.
	r2 := e.Push(ev(2, 1, 1000, 200))
	if len(r2.Committed) != 1 || r2.Committed[0].ByteOff != 100 {
		t.Fatalf("want first event committed, got %+v", r2.Committed)
	}
	if wm := e.Watermark(); wm != 500_000_000 {
		t.Fatalf("watermark=%d want 500ms", wm)
	}
	if len(e.Pending()) != 1 {
		t.Fatalf("1000ms event must remain pending")
	}
}

func TestLateEventAndReplay(t *testing.T) {
	e := newTestEngine()
	e.Push(ev(1, 1, 0, 100))
	e.Push(ev(2, 1, 1000, 200)) // watermark -> 500ms, commits event 1
	if e.Watermark() != 500_000_000 {
		t.Fatalf("setup watermark=%d", e.Watermark())
	}
	// Late event at 100ms (below watermark) but inside replay window.
	late := ev(3, 2, 100, 300)
	r := e.Push(late)
	if !r.Late || e.LateCount() != 1 {
		t.Fatalf("event should be classified late: %+v", r)
	}
	committed := e.Committed()
	// Stable order: 0ms(t1), 100ms(t2), then pending 1000ms excluded.
	if len(committed) != 2 {
		t.Fatalf("want 2 committed incl. late, got %d", len(committed))
	}
	if committed[0].TimeNS != 0 || committed[1].TimeNS != 100_000_000 {
		t.Fatalf("late event not ordered correctly: %+v", committed)
	}
	// Replay window anchored at a checkpoint before the late event.
	w, err := e.Replay(late, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Events) != 2 || w.Events[1].ByteOff != 300 {
		t.Fatalf("replay window wrong: %+v", w.Events)
	}
	// Checkpoint after the late event must reject the replay.
	if _, err := e.Replay(late, 200_000_000); err == nil {
		t.Fatal("replay past checkpoint must fail")
	} else if me, ok := clock.AsMediaError(err); !ok || me.Category != clock.CatLateEvent {
		t.Fatalf("want CatLateEvent, got %v", err)
	}
}

func TestLateOutsideWindowRejected(t *testing.T) {
	e := newTestEngine()
	e.Push(ev(1, 1, 0, 100))
	e.Push(ev(2, 1, 5000, 200)) // watermark -> 4500ms
	// Event 1000ms in the past: distance 3500ms > 2000ms window.
	old := ev(3, 1, 1000, 300)
	r := e.Push(old)
	if !r.Rejected || r.Reason != clock.CatLateEvent {
		t.Fatalf("want rejected late event, got %+v", r)
	}
}

func TestDuplicateIsIdempotent(t *testing.T) {
	e := newTestEngine()
	e.Push(ev(1, 1, 0, 100))
	e.Push(ev(2, 1, 1000, 200))
	// Same byte offset fed again with a fresh arrival sequence.
	dup := ev(99, 1, 0, 100)
	r := e.Push(dup)
	if !r.Duplicate || e.DupCount() != 1 {
		t.Fatalf("duplicate not detected: %+v", r)
	}
	if len(e.Committed()) != 1 {
		t.Fatal("duplicate must not change committed set")
	}
}

func TestDeterministicRepeatedInput(t *testing.T) {
	build := func() []Event {
		e := newTestEngine()
		inputs := []Event{
			ev(1, 1, 0, 100),
			ev(2, 2, 300, 110),
			ev(3, 1, 1000, 120),
			ev(4, 2, 100, 130), // late
			ev(5, 1, 2000, 140),
			ev(3, 1, 1000, 120), // duplicate
		}
		for _, in := range inputs {
			e.Push(in)
		}
		e.Flush()
		return e.Committed()
	}
	a, b := build(), build()
	if len(a) != len(b) {
		t.Fatalf("length mismatch %d vs %d", len(a), len(b))
	}
	for i := range a {
		if StableKey(a[i]) != StableKey(b[i]) || a[i].ByteOff != b[i].ByteOff {
			t.Fatalf("run mismatch at %d:\n%+v\n%+v", i, a[i], b[i])
		}
	}
	// Expect stable time order: 0,100(late),300,1000,2000 (dup dropped).
	want := []int64{0, 100, 300, 1000, 2000}
	if len(a) != len(want) {
		t.Fatalf("got %d events want %d", len(a), len(want))
	}
	for i, w := range want {
		if a[i].TimeNS != w*1_000_000 {
			t.Fatalf("position %d time=%d want %dms", i, a[i].TimeNS, w)
		}
	}
}

func TestFlushCommitsAll(t *testing.T) {
	e := newTestEngine()
	e.Push(ev(1, 1, 0, 100))
	e.Push(ev(2, 1, 250, 110))
	if len(e.Pending()) != 2 {
		t.Fatal("both events pending before flush")
	}
	flushed := e.Flush()
	if len(flushed) != 2 || len(e.Pending()) != 0 {
		t.Fatal("flush must commit every pending event")
	}
	if e.Watermark() != 250_000_000 {
		t.Fatalf("watermark after flush=%d", e.Watermark())
	}
}

func TestReplayFromFullWindow(t *testing.T) {
	e := newTestEngine()
	for i, ms := range []int64{0, 200, 400, 800, 1200} {
		e.Push(ev(int64(i+1), 1, ms, int64(100+i)))
	}
	e.Flush()
	w := e.ReplayFrom(400_000_000)
	if len(w.Events) != 3 { // 400, 800, 1200
		t.Fatalf("want 3 events from 400ms, got %d", len(w.Events))
	}
	if w.Events[0].TimeNS != 400_000_000 {
		t.Fatalf("replay-from boundary wrong: %d", w.Events[0].TimeNS)
	}
}
