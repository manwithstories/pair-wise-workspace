package timeline

import (
	"math"
	"testing"

	"mediq/internal/model"
)

var tb90 = model.Timebase{Num: 90000, Den: 1}

// pushAll feeds frames and collects emitted events.
func pushAll(r *Repairer, frames ...*model.Frame) []Event {
	var out []Event
	for _, fr := range frames {
		out = append(out, r.Push(fr)...)
	}
	return append(out, r.Flush()...)
}

func frame(seq, ts uint64) *model.Frame {
	return &model.Frame{Seq: seq, Timestamp: ts, Timebase: tb90, FrameOffset: int64(seq * 100)}
}

func indexed(evs []Event) []*model.Frame {
	var out []*model.Frame
	for _, ev := range evs {
		if ev.Status == StatusIndexed {
			out = append(out, ev.Frame)
		}
	}
	return out
}

func TestReorderWindowOverflowEmitsOldest(t *testing.T) {
	r := New(Options{ReorderWindow: 2})
	r.Push(frame(1, 0))
	r.Push(frame(2, 3000))
	// Frame 3 is missing. Frames 4 and 5 fill the window; the arrival of 6
	// overflows it and forces the oldest buffered frame out.
	r.Push(frame(4, 9000))
	r.Push(frame(5, 12000))
	evs := r.Push(frame(6, 15000))
	var got []uint64
	for _, ev := range evs {
		if ev.Status == StatusIndexed {
			got = append(got, ev.Frame.Seq)
		}
	}
	if len(got) == 0 || got[0] != 4 {
		t.Fatalf("expected frame 4 to be forced out, got %+v", evs)
	}
	if r.PendingCount() > 2 {
		t.Fatalf("pending %d exceeds window", r.PendingCount())
	}
}

func TestFirstFrameMayStartAtAnySequence(t *testing.T) {
	r := New(Options{})
	// Capture begins mid-stream: the first frame seen is 3 of a larger run.
	evs := pushAll(r, frame(3, 0), frame(4, 3000), frame(5, 6000))
	got := indexed(evs)
	if len(got) != 3 || got[0].Seq != 3 {
		t.Fatalf("got %+v", got)
	}
	if got[1].MediaTime-got[0].MediaTime != 3000 {
		t.Fatalf("media step %d", got[1].MediaTime-got[0].MediaTime)
	}
	// A late arrival of a pre-start sequence is a duplicate, not a reopening.
	evs = r.Push(frame(1, 0))
	if len(evs) != 1 || evs[0].Fault.Code == "" {
		t.Fatalf("late pre-start frame: %+v", evs)
	}
}

func TestSequentialMediaTimes(t *testing.T) {
	r := New(Options{})
	var frames []*model.Frame
	for i := 0; i < 10; i++ {
		frames = append(frames, frame(uint64(i+1), uint64(i)*3000))
	}
	evs := pushAll(r, frames...)
	got := indexed(evs)
	if len(got) != 10 {
		t.Fatalf("indexed %d", len(got))
	}
	for i, fr := range got {
		if fr.MediaTime != int64(i)*3000 {
			t.Fatalf("frame %d media %d", i, fr.MediaTime)
		}
	}
}

func TestTimestampWrapRepaired(t *testing.T) {
	r := New(Options{WrapModulus: 1 << 32})
	// Frames 1..5 ascend, frame 6 wraps back to a small raw value (as a 32-bit
	// capture clock would).
	ts := []uint64{1<<32 - 9000, 1<<32 - 6000, 1<<32 - 3000, 1<<32 - 1000, 0, 3000, 6000}
	var frames []*model.Frame
	for i, v := range ts {
		frames = append(frames, frame(uint64(i+1), v))
	}
	evs := pushAll(r, frames...)
	got := indexed(evs)
	if len(got) != len(ts) {
		t.Fatalf("indexed %d of %d", len(got), len(ts))
	}
	if r.Wraps() != 1 {
		t.Fatalf("wraps %d", r.Wraps())
	}
	for i := 1; i < len(got); i++ {
		if got[i].MediaTime <= got[i-1].MediaTime {
			t.Fatalf("media not monotonic at %d: %d <= %d", i, got[i].MediaTime, got[i-1].MediaTime)
		}
		if got[i].MediaTime-got[i-1].MediaTime > 90000 {
			t.Fatalf("wrap left a %d-tick jump at frame %d", got[i].MediaTime-got[i-1].MediaTime, i)
		}
	}
}

func TestDuplicateSequenceQuarantined(t *testing.T) {
	r := New(Options{})
	a := frame(1, 0)
	b := frame(2, 3000)
	dup := frame(2, 3000)
	evs := pushAll(r, a, b, dup)
	if n := len(indexed(evs)); n != 2 {
		t.Fatalf("indexed %d", n)
	}
	var quarantined []Event
	for _, ev := range evs {
		if ev.Status == StatusQuarantined {
			quarantined = append(quarantined, ev)
		}
	}
	if len(quarantined) != 1 || quarantined[0].Fault.Code != model.CodeDuplicateSeq {
		t.Fatalf("got %+v", quarantined)
	}
}

func TestOutOfOrderReordered(t *testing.T) {
	r := New(Options{})
	// Emit 1, 3, 2, 4: the window must restore sequence order.
	evs := pushAll(r, frame(1, 0), frame(3, 6000), frame(2, 3000), frame(4, 9000))
	got := indexed(evs)
	if len(got) != 4 {
		t.Fatalf("indexed %d", len(got))
	}
	for i, fr := range got {
		if fr.Seq != uint64(i+1) {
			t.Fatalf("position %d holds frame %d", i, fr.Seq)
		}
	}
	if got[0].MediaTime >= got[1].MediaTime || got[1].MediaTime >= got[2].MediaTime {
		t.Fatal("media times not monotonic after reorder")
	}
}

func TestReorderWindowBound(t *testing.T) {
	r := New(Options{ReorderWindow: 4})
	var frames []*model.Frame
	for i := 1; i <= 100; i++ {
		frames = append(frames, frame(uint64(i), uint64(i)*3000))
	}
	// Push ascending but delay frame 1: with the window at 4 the repairer must
	// not buffer the whole stream waiting for it.
	for _, fr := range frames[1:20] {
		r.Push(fr)
		if r.PendingCount() > 4 {
			t.Fatalf("pending %d exceeds window", r.PendingCount())
		}
	}
}

func TestLateFrameQuarantined(t *testing.T) {
	r := New(Options{ReorderWindow: 2})
	pushAll(r, frame(1, 0), frame(2, 3000), frame(3, 6000), frame(4, 9000))
	// Frame 2 arriving now is behind the emitted horizon.
	evs := r.Push(frame(2, 3000))
	if len(evs) != 1 || evs[0].Status != StatusQuarantined || evs[0].Fault.Code != model.CodeOutOfOrder {
		t.Fatalf("got %+v", evs)
	}
	if evs[0].Fault.Offset != 200 {
		t.Fatalf("offset %d", evs[0].Fault.Offset)
	}
}

func TestSkipAdvancesGap(t *testing.T) {
	r := New(Options{ReorderWindow: 4})
	pushAll(r, frame(1, 0), frame(2, 3000))
	r.Skip(3) // frame 3 was corrupt and dropped
	evs := r.Push(frame(4, 9000))
	got := indexed(evs)
	if len(got) != 1 || got[0].Seq != 4 {
		t.Fatalf("got %+v", got)
	}
	if r.NextSeq() != 5 {
		t.Fatalf("next seq %d", r.NextSeq())
	}
}

func TestBackwardsDriftClamped(t *testing.T) {
	r := New(Options{DriftTolerance: 450})
	evs := pushAll(r,
		frame(1, 3000),
		frame(2, 6000),
		frame(3, 5900), // 100 ticks back: jitter
		frame(4, 9000),
	)
	got := indexed(evs)
	if len(got) != 4 {
		t.Fatalf("indexed %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].MediaTime <= got[i-1].MediaTime {
			t.Fatalf("media not strictly increasing at %d", i)
		}
	}
	if !evs[2].Adjusted {
		t.Fatal("backwards drift should be flagged as adjusted")
	}
}

func TestLargeBackwardsJumpQuarantined(t *testing.T) {
	r := New(Options{DriftTolerance: 450})
	evs := pushAll(r,
		frame(1, 30000),
		frame(2, 60000),
		frame(3, 1000), // far backwards, no wrap plausibility
	)
	got := indexed(evs)
	if len(got) != 2 {
		t.Fatalf("indexed %d", len(got))
	}
	var q *Event
	for i := range evs {
		if evs[i].Status == StatusQuarantined {
			q = &evs[i]
		}
	}
	if q == nil || q.Fault.Code != model.CodeUnrepairableTime {
		t.Fatalf("got %+v", q)
	}
}

func TestForwardJumpQuarantined(t *testing.T) {
	r := New(Options{})
	// 3000 ticks per frame, then a sudden 10-second gap: ambiguous (dropped
	// frames or a timestamp error), so it is isolated.
	evs := pushAll(r,
		frame(1, 0), frame(2, 3000), frame(3, 6000), frame(4, 9000),
		frame(5, 9000+900000),
		frame(6, 9000+903000),
	)
	if n := len(indexed(evs)); n != 4 {
		t.Fatalf("indexed %d, want 4", n)
	}
	var q *Event
	for i := range evs {
		if evs[i].Status == StatusQuarantined {
			q = &evs[i]
		}
	}
	if q == nil || q.Fault.Code != model.CodeUnrepairableTime {
		t.Fatalf("got %+v", q)
	}
}

func TestBadTimebaseQuarantined(t *testing.T) {
	r := New(Options{})
	fr := frame(1, 0)
	fr.Timebase = model.Timebase{}
	evs := r.Push(fr)
	if len(evs) != 1 || evs[0].Fault.Code != model.CodeBadTimebase {
		t.Fatalf("got %+v", evs)
	}
}

func TestNTSCRationalTimebase(t *testing.T) {
	tb := model.Timebase{Num: 30000, Den: 1001}
	r := New(Options{})
	var frames []*model.Frame
	// Each frame advances the 29.97 Hz clock by one tick.
	for i := 0; i < 30; i++ {
		frames = append(frames, &model.Frame{Seq: uint64(i + 1), Timestamp: uint64(i), Timebase: tb})
	}
	got := indexed(pushAll(r, frames...))
	if len(got) != 30 {
		t.Fatalf("indexed %d", len(got))
	}
	// 29 frames at 3003.003 ticks each: the error must stay under one frame.
	want := int64(float64(29) * 90000 * 1001 / 30000)
	if diff := got[29].MediaTime - want; diff < -3003 || diff > 3003 {
		t.Fatalf("frame 29 offset %d ticks from rational time", diff)
	}
}

func TestWrapClampedAtHighEnd(t *testing.T) {
	r := New(Options{WrapModulus: 1 << 32})
	// Timestamps sit just below 2^32 and wrap repeatedly.
	var frames []*model.Frame
	base := uint64(math.MaxUint32 - 4)
	for i := 0; i < 6; i++ {
		v := (base + uint64(i)*3000) % (1 << 32)
		frames = append(frames, frame(uint64(i+1), v))
	}
	got := indexed(pushAll(r, frames...))
	if len(got) != 6 {
		t.Fatalf("indexed %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].MediaTime-got[i-1].MediaTime != 3000 {
			t.Fatalf("frame %d step %d", i, got[i].MediaTime-got[i-1].MediaTime)
		}
	}
}

func TestSnapshotRestore(t *testing.T) {
	r := New(Options{})
	pushAll(r, frame(1, 0), frame(2, 3000), frame(3, 6000))
	snap := r.Snapshot()

	r2 := New(Options{})
	r2.Restore(snap)
	// The next frame must continue from the restored state.
	got := indexed(pushAll(r2, frame(4, 9000)))
	if len(got) != 1 || got[0].MediaTime != 9000 {
		t.Fatalf("got %+v", got)
	}

	// A restored repairer must also keep the old sequence numbers indexed.
	if evs := r2.Push(frame(2, 3000)); len(evs) != 1 || evs[0].Fault.Code != model.CodeOutOfOrder {
		t.Fatalf("stale frame after restore: %+v", evs)
	}
}

func TestPendingFloor(t *testing.T) {
	r := New(Options{ReorderWindow: 8})
	start := func(seq uint64) *model.Frame {
		fr := frame(seq, seq*3000)
		fr.StartOffset = int64(seq * 100)
		return fr
	}
	r.Push(start(1))
	r.Push(start(2))
	r.Push(start(4)) // 3 is missing, so 4 stays pending
	floor, ok := r.PendingFloor()
	if !ok {
		t.Fatal("expected pending frames")
	}
	if floor != 400 {
		t.Fatalf("floor %d", floor)
	}
}
