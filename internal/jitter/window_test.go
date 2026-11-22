package jitter

import (
	"testing"

	"mediakernel/internal/clock"
	"mediakernel/internal/model"
)

func ev(kind model.Kind, stream uint32, seq uint16, at int64) *model.Event {
	return &model.Event{
		Kind: kind, Stream: stream, Seq: seq,
		EventTime: clock.Ticks(at), Arrival: clock.Ticks(at + 100),
		PayloadHash: uint64(seq),
	}
}

func TestReorderAndDrain(t *testing.T) {
	w := New(Config{MaxEvents: 8}, model.NewLog(64))
	// Push in deliberately shuffled media-time order.
	for _, seq := range []uint16{3, 1, 4, 2, 0} {
		w.Push(ev(model.KindAudio, 1, seq, int64(seq)*100))
	}
	if w.Reordered() != 3 {
		t.Fatalf("reordered count: %d", w.Reordered())
	}
	out := w.DrainUpTo(250)
	if len(out) != 3 {
		t.Fatalf("drain count: %d", len(out))
	}
	for i := 1; i < len(out); i++ {
		if out[i].EventTime < out[i-1].EventTime {
			t.Fatal("drained events not in event-time order")
		}
	}
	if w.Len() != 2 {
		t.Fatalf("remaining: %d", w.Len())
	}
	// Same event time: stream then seq must break ties deterministically.
	w2 := New(DefaultConfig(), model.NewLog(64))
	w2.Push(ev(model.KindVideo, 2, 9, 500))
	w2.Push(ev(model.KindAudio, 1, 9, 500))
	w2.Push(ev(model.KindAudio, 1, 1, 500))
	got := w2.DrainUpTo(500)
	if got[0].Stream != 1 || got[0].Seq != 1 || got[2].Stream != 2 {
		t.Fatalf("tie-break order wrong: %+v", got)
	}
}

func TestBoundedEviction(t *testing.T) {
	log := model.NewLog(64)
	w := New(Config{MaxEvents: 3}, log)
	for seq := uint16(0); seq < 6; seq++ {
		w.Push(ev(model.KindAudio, 1, seq, int64(seq)*10))
	}
	if w.Len() != 3 {
		t.Fatalf("window exceeded cap: %d", w.Len())
	}
	if w.Evicted() != 3 {
		t.Fatalf("eviction count: %d", w.Evicted())
	}
	if log.CountBy(model.CatOverflow) != 3 {
		t.Fatal("evictions must be recorded as overflow faults")
	}
	// Oldest events are the ones gone.
	got := w.DrainUpTo(1 << 40)
	if got[0].Seq != 3 {
		t.Fatalf("expected oldest survivors from seq 3, got %d", got[0].Seq)
	}
}

func TestSnapshotDoesNotMutate(t *testing.T) {
	w := New(DefaultConfig(), model.NewLog(16))
	w.Push(ev(model.KindAudio, 1, 0, 10))
	snap := w.Snapshot()
	snap[0] = nil
	if w.Peek() == nil {
		t.Fatal("snapshot aliased internal storage")
	}
}
