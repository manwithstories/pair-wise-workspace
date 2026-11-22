package metadata

import (
	"testing"

	"mediacheck/internal/model"
)

func TestAlignStableOrdering(t *testing.T) {
	windows := []TrackWindow{{Track: "v", StartMs: 0, EndMs: 5000}}
	events := []Event{
		{Kind: KindSubtitle, Track: "v", StartMs: 500, Seq: 0},
		{Kind: KindChapter, Track: "v", StartMs: 500, Seq: 1},
		{Kind: KindEMSG, Track: "v", StartMs: 500, Seq: 2},
		{Kind: KindID3, Track: "v", StartMs: 100, Seq: 3},
	}
	rep := Align(events, windows, DefaultOptions())
	if len(rep.Events) != 4 {
		t.Fatalf("want 4 aligned, got %d", len(rep.Events))
	}
	// ID3(100) first; at 500 the rank order is chapter < emsg < subtitle.
	want := []Kind{KindID3, KindChapter, KindEMSG, KindSubtitle}
	for i, w := range want {
		if rep.Events[i].Kind != w {
			t.Fatalf("position %d want %s got %s", i, w, rep.Events[i].Kind)
		}
	}
}

func TestAlignRepeatedRunIdentical(t *testing.T) {
	windows := []TrackWindow{{Track: "v", StartMs: 0, EndMs: 5000}}
	events := []Event{
		{Kind: KindSEI, Track: "v", StartMs: 900, Seq: 0},
		{Kind: KindID3, Track: "v", StartMs: 100, Seq: 1},
		{Kind: KindEMSG, Track: "v", StartMs: 100, Seq: 2},
	}
	first := Align(events, windows, DefaultOptions())
	second := Align(events, windows, DefaultOptions())
	for i := range first.Events {
		if first.Events[i] != second.Events[i] {
			t.Fatalf("alignment not deterministic at %d", i)
		}
	}
}

func TestOrphanEventRetainedAsFault(t *testing.T) {
	windows := []TrackWindow{{Track: "v", StartMs: 0, EndMs: 1000}}
	events := []Event{
		{Kind: KindID3, Track: "v", StartMs: 500, Seq: 0},
		{Kind: KindID3, Track: "v", StartMs: 9999, Seq: 1}, // far outside
	}
	rep := Align(events, windows, DefaultOptions())
	if len(rep.Events) != 1 {
		t.Fatalf("only anchored event should align, got %d", len(rep.Events))
	}
	if len(rep.Faults) != 1 || rep.Faults[0].Kind != model.KindOrphanEvent {
		t.Fatalf("orphan must be retained as a fault: %+v", rep.Faults)
	}
}

func TestNoWindowsEverythingOrphan(t *testing.T) {
	rep := Align([]Event{{Kind: KindSEI, StartMs: 0}}, nil, DefaultOptions())
	if len(rep.Events) != 0 || len(rep.Faults) != 1 {
		t.Fatal("events without any track window must all be orphans")
	}
}
