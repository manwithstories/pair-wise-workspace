package timeline

import (
	"testing"

	"example.com/medialab/archive/internal/model"
	"example.com/medialab/archive/internal/recovery"
)

func ev(seq uint32, start, dur int64) *model.Event {
	return &model.Event{
		ID:         model.PacketID{Stream: "s", Seq: seq},
		ExtTimeUS:  start,
		DurationUS: dur,
		PayloadSHA: "x",
	}
}

func TestWatermarkAdvancesMonotonically(t *testing.T) {
	var w Watermark
	w.lateWindowUS = 2_000_000
	if w.Set() {
		t.Fatal("empty watermark reported set")
	}
	w.Observe(5_000_000)
	if w.Value() != 3_000_000 {
		t.Fatalf("value = %d", w.Value())
	}
	w.Observe(1_000_000) // out-of-order arrival must not rewind
	if w.Value() != 3_000_000 {
		t.Fatalf("value rewound to %d", w.Value())
	}
	if !w.Late(2_900_000, 3_000_000) {
		t.Fatal("event at watermark should be late")
	}
	if w.Late(3_000_001, 3_100_000) {
		t.Fatal("event after watermark should be in window")
	}
}

func TestWatermarkFirstEventClampsAtZero(t *testing.T) {
	var w Watermark
	w.lateWindowUS = 2_000_000
	w.Observe(1_000_000)
	if w.Value() != 0 {
		t.Fatalf("value = %d", w.Value())
	}
}

func TestSortedEventsDeterministic(t *testing.T) {
	a := &model.Event{ID: model.PacketID{Stream: "b", Seq: 1}, ExtTimeUS: 5}
	b := &model.Event{ID: model.PacketID{Stream: "a", Seq: 9}, ExtTimeUS: 5}
	c := &model.Event{ID: model.PacketID{Stream: "a", Seq: 3}, ExtTimeUS: 5}
	got := SortedEvents([]*model.Event{a, b, c})
	if got[0] != c || got[1] != b || got[2] != a {
		t.Fatalf("order = %v %v %v", got[0].ID, got[1].ID, got[2].ID)
	}
	// Same input in another order must produce the same output order.
	got2 := SortedEvents([]*model.Event{c, a, b})
	for i := range got {
		if got[i] != got2[i] {
			t.Fatal("sort is not stable across input order")
		}
	}
}

func TestGapsSilenceFillAndSeqHole(t *testing.T) {
	events := []*model.Event{ev(10, 0, 20000), ev(13, 100000, 20000)}
	decisions, stats := Gaps(events, nil, 1_000_000)
	if len(decisions) != 1 || !decisions[0].Recoverable {
		t.Fatalf("decisions = %+v", decisions)
	}
	if decisions[0].StartUS != 20000 || decisions[0].DurationUS != 80000 {
		t.Fatalf("decision = %+v", decisions[0])
	}
	if stats.LossHoles != 1 || stats.RecoveredHoles != 1 || stats.EstimatedUS != 80000 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestGapsContiguousAndOverlapHaveNoHole(t *testing.T) {
	events := []*model.Event{ev(1, 0, 20000), ev(2, 20000, 20000), ev(3, 30000, 20000)}
	decisions, stats := Gaps(events, nil, 1_000_000)
	if len(decisions) != 0 || stats.LossHoles != 0 {
		t.Fatalf("decisions=%+v stats=%+v", decisions, stats)
	}
}

func TestGapsCorruptSkipped(t *testing.T) {
	corrupt := ev(11, 20000, -1)
	events := []*model.Event{ev(10, 0, 20000), corrupt, ev(12, 40000, 20000)}
	decisions, stats := Gaps(events, map[uint32]model.ErrorKind{11: model.KindCorrupt}, 1_000_000)
	if len(decisions) != 1 || !decisions[0].Recoverable {
		t.Fatalf("bounded corrupt hole should be silence filled: %+v", decisions)
	}
	if stats.CorruptHoles != 1 || stats.LossHoles != 0 {
		t.Fatalf("corrupt hole attributed to loss: %+v", stats)
	}
	// Without the quarantine evidence the same hole is ordinary loss.
	_, stats2 := Gaps(events, nil, 1_000_000)
	if stats2.LossHoles != 1 || stats2.CorruptHoles != 0 {
		t.Fatalf("unknown hole stats = %+v", stats2)
	}
}

func TestConfigValidate(t *testing.T) {
	good := Config{SegmentUS: 10_000_000, LateWindowUS: 2_000_000, MaxPacketUS: 120_000}
	if good.Validate() != nil {
		t.Fatal(good.Validate())
	}
	for _, bad := range []Config{
		{},
		{SegmentUS: 1, MaxPacketUS: 1, LateWindowUS: 2},
		{SegmentUS: 1, MaxPacketUS: 0},
	} {
		if bad.Validate() == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestRecoveryActionNamed(t *testing.T) {
	if recovery.ActionSilenceFill == "" || recovery.ActionUnrecoverable == "" {
		t.Fatal("empty action")
	}
}
