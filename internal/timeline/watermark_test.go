package timeline

import (
	"testing"

	"github.com/clearliu/mediq/internal/core"
)

func event(i int64, span int64) Event {
	return Event{Sequence: i, Start: core.Time(i * span), End: core.Time((i + 1) * span)}
}

func TestWatermarkCommitsIntervals(t *testing.T) {
	w := New(Config{IntervalSamples: 10, RetentionIntervals: 4})
	for i := int64(0); i < 3; i++ {
		rs, replay, rejected := w.Observe(event(i, 10))
		if replay != nil || rejected {
			t.Fatalf("event %d unexpected replay=%v rejected=%v", i, replay, rejected)
		}
		if len(rs) != 1 || rs[0].Start != core.Time(i*10) || rs[0].End != core.Time((i+1)*10) {
			t.Fatalf("event %d ranges=%+v", i, rs)
		}
	}
	if w.CommittedEnd() != 30 {
		t.Fatalf("committed=%d, want 30", w.CommittedEnd())
	}
}

func TestWatermarkRespectsLateness(t *testing.T) {
	w := New(Config{IntervalSamples: 10, LatenessSamples: 5})
	if rs, _, _ := w.Observe(event(0, 10)); len(rs) != 0 {
		t.Fatalf("end=10 lateness=5 should not commit interval: %+v", rs)
	}
	ev := Event{Sequence: 1, Start: 10, End: 15}
	rs, _, _ := w.Observe(ev)
	if len(rs) != 1 || rs[0].End != 10 {
		t.Fatalf("watermark=10 should commit first interval only: %+v", rs)
	}
}

func TestLateEventReplaysFromRetainedCheckpoint(t *testing.T) {
	w := New(Config{IntervalSamples: 10, RetentionIntervals: 3})
	for i := int64(0); i < 5; i++ {
		w.Observe(event(i, 10))
	}
	// Retention=3 keeps intervals 2,3,4.
	cps := w.Checkpoints()
	if len(cps) != 3 || cps[0].Interval != 2 {
		t.Fatalf("checkpoints=%+v, want intervals 2..4", cps)
	}

	rs, replay, rejected := w.Observe(event(2, 10))
	if rejected || replay == nil {
		t.Fatalf("late in retention rejected=%v replay=%v", rejected, replay)
	}
	if replay.Token != "deterministic-cp-000000000002" {
		t.Fatalf("unexpected deterministic token %q", replay.Token)
	}
	if len(rs) != 3 || rs[0].Index != 2 || rs[2].Index != 4 {
		t.Fatalf("recomputed ranges=%+v, want 2..4", rs)
	}
	if w.LateReplays != 1 {
		t.Fatalf("replays=%d, want 1", w.LateReplays)
	}
}

func TestLateEventBeyondRetentionRejected(t *testing.T) {
	w := New(Config{IntervalSamples: 10, RetentionIntervals: 2})
	for i := int64(0); i < 5; i++ {
		w.Observe(event(i, 10))
	}
	before := w.CommittedEnd()
	_, replay, rejected := w.Observe(event(0, 10))
	if !rejected || replay != nil {
		t.Fatalf("ancient event rejected=%v replay=%v", rejected, replay)
	}
	if w.CommittedEnd() != before || w.LateExpired != 1 {
		t.Fatalf("state changed: before=%d after=%d expired=%d", before, w.CommittedEnd(), w.LateExpired)
	}
}

func TestFlushCommitsPartialTail(t *testing.T) {
	w := New(Config{IntervalSamples: 10})
	w.Observe(Event{Sequence: 0, Start: 0, End: 10})
	w.Observe(Event{Sequence: 1, Start: 10, End: 14})
	rs := w.Flush()
	if len(rs) != 1 || rs[0].Start != 10 || rs[0].End != 14 {
		t.Fatalf("flush ranges=%+v, want 10..14", rs)
	}
}
