package events

import (
	"math"
	"testing"

	"fieldcut/internal/clock"
)

func TestAlignOverflowPreservesOriginal(t *testing.T) {
	evs, errs := Align([]RawEvent{{
		ID: "overflow", TrackID: 33, Kind: KindID3,
		TimeBaseNum: 1, TimeBaseDen: 1, StartTicks: math.MaxInt64, EndTicks: math.MaxInt64,
	}})
	if len(evs) != 0 || len(errs) != 1 {
		t.Fatalf("evs=%d errs=%d", len(evs), len(errs))
	}
	if errs[0].Code != clock.CodeOverflow || errs[0].TrackID != 33 || errs[0].OriginalTimestamp != math.MaxInt64 {
		t.Fatalf("overflow error lost original values: %#v", errs[0])
	}
}

func TestAssignCrossSegmentWithoutLossOrDuplicateGlobal(t *testing.T) {
	events := []Event{{
		ID: "sub-1", TrackID: 30, Kind: KindSubtitle,
		Interval: clock.Interval{Start: 500, End: 1500},
	}}
	segments := []clock.Interval{{Start: 0, End: 1000}, {Start: 1000, End: 2000}}
	assignments, err := Assign(events, segments)
	if err != nil {
		t.Fatal(err)
	}
	if len(assignments) != 2 {
		t.Fatalf("a spanning event should have two local assignments: %#v", assignments)
	}
	for _, a := range assignments {
		if len(a.SegmentIDs) != 2 || a.SegmentIDs[0] != 0 || a.SegmentIDs[1] != 1 {
			t.Fatalf("global segment IDs wrong: %#v", a)
		}
	}
	seen := map[string]int{}
	for _, a := range assignments {
		seen[a.EventID]++
	}
	if len(seen) != 1 || seen["sub-1"] != 2 {
		t.Fatalf("global event should remain unique with two local spans: %v", seen)
	}
}

func TestCrossTrackSkew(t *testing.T) {
	evs := []Event{
		{ID: "a", GroupID: "g", TrackID: 30, Interval: clock.Interval{Start: 100, End: 200}},
		{ID: "b", GroupID: "g", TrackID: 31, Interval: clock.Interval{Start: 180, End: 280}},
	}
	errs := CrossTrackSkew(evs, 50)
	if len(errs) != 1 || errs[0].Code != clock.CodeBadInterval {
		t.Fatalf("errs=%#v", errs)
	}
	if errs[0].StartNs != 100 || errs[0].EndNs != 180 {
		t.Fatalf("skew interval wrong: %#v", errs[0])
	}
}
