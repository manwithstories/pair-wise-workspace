package timeline

import (
	"testing"

	"mediakernel/internal/clock"
	"mediakernel/internal/model"
)

func TestWatermarkAdvanceAndSeal(t *testing.T) {
	wm := NewWatermark(model.NewLog(16))
	if wm.IsSealed(clock.Ticks(0)) {
		t.Fatal("fresh watermark seals nothing")
	}
	wm.Advance(clock.Milliseconds(100))
	if !wm.IsSealed(clock.Milliseconds(100)) || wm.IsSealed(clock.Milliseconds(101)) {
		t.Fatal("sealing boundary wrong")
	}
	span := wm.Advance(clock.Milliseconds(250))
	if span.Empty || span.From != clock.Milliseconds(100) || span.To != clock.Milliseconds(250) {
		t.Fatalf("sealed span wrong: %+v", span)
	}
	// Backward movement is a no-op.
	back := wm.Advance(clock.Milliseconds(50))
	if !back.Empty || wm.Mark() != clock.Milliseconds(250) {
		t.Fatal("watermark must move forward only")
	}
}

func TestReplayerWindowAndDeterminism(t *testing.T) {
	log := model.NewLog(64)
	window := clock.Milliseconds(200)
	r := NewReplayer(window, 64, log)
	mark := clock.Milliseconds(500)

	// Event 300ms old: outside the 200ms window -> rejected.
	old := &model.Event{Stream: 1, Seq: 1, EventTime: clock.Milliseconds(200), PayloadHash: 11}
	d := r.Offer(old, mark)
	if !d.Rejected || d.Replay {
		t.Fatalf("outside-window decision wrong: %+v", d)
	}
	// Event 100ms old: inside window -> replay.
	late := &model.Event{Stream: 1, Seq: 2, EventTime: clock.Milliseconds(400), PayloadHash: 22}
	d = r.Offer(late, mark)
	if !d.Replay || d.Slot != clock.Milliseconds(400) {
		t.Fatalf("inside-window decision wrong: %+v", d)
	}
	if r.ReplayCount() != 1 || r.RejectedCount() != 1 {
		t.Fatal("replay/reject counts wrong")
	}
	if log.CountBy(model.CatLate) != 2 {
		t.Fatal("late events must be classified")
	}

	// Seal a deterministic ordered history on a fresh replayer so that
	// window-based aging cannot remove the interval under test. Sealing up to
	// 500ms ages out everything below 300ms and retains exactly the replay
	// interval [300ms, 500ms].
	r2 := NewReplayer(window, 64, log)
	for i := 0; i <= 5; i++ {
		r2.Seal(clock.Milliseconds(int64(100*i)), 1, uint16(i), uint64(1000+i), i)
	}
	items := r2.Recompute(clock.Milliseconds(300))
	// slot=300ms, window ends at 500ms; items below 300ms have aged out of
	// the retained history, leaving 300,400,500.
	if len(items) != 3 {
		t.Fatalf("recompute scope wrong: %d items", len(items))
	}
	for i, it := range items {
		wantOrder := 3 + i
		if it.Order != wantOrder {
			t.Fatalf("recompute order not deterministic at %d: %d", i, it.Order)
		}
	}
	// Two independent replay runs over the same history must match exactly.
	again := r2.Recompute(clock.Milliseconds(300))
	for i := range items {
		if items[i] != again[i] {
			t.Fatal("replay must be reproducible across runs")
		}
	}
}

func TestReplayerHistoryBounded(t *testing.T) {
	r := NewReplayer(clock.Milliseconds(1000), 4, model.NewLog(16))
	for i := 0; i < 10; i++ {
		// Keep times inside the relative window so capacity, not age, trims.
		r.Seal(clock.Milliseconds(int64(900+i)), 1, uint16(i), uint64(i), i)
	}
	if r.HistoryLen() != 4 {
		t.Fatalf("history cap not enforced: %d", r.HistoryLen())
	}
}
