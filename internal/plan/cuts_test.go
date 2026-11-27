package plan

import (
	"testing"

	"aircheck/internal/fault"
	"aircheck/internal/scan"
)

func scanRes(dur, seg int64, events ...scan.Event) *scan.Result {
	return &scan.Result{
		Package: "t", SegmentTicks: seg,
		Renditions: []scan.Rendition{{Name: "a", DurationTicks: dur}, {Name: "b", DurationTicks: dur}},
		Events:     events,
	}
}

func TestBuildCuts(t *testing.T) {
	p, err := Build(scanRes(450000, 180000)) // 5 s, 2 s segments
	if err != nil {
		t.Fatal(err)
	}
	if p.NumSegments() != 3 {
		t.Fatalf("cuts %v", p.Cuts)
	}
	want := []int64{0, 180000, 360000, 450000}
	for i, c := range want {
		if p.Cuts[i] != c {
			t.Fatalf("cut %d = %d want %d", i, p.Cuts[i], c)
		}
	}
}

func TestEventOwnershipUnique(t *testing.T) {
	evs := []scan.Event{
		{Type: "id3", Tick: 0, SegmentID: -1},
		{Type: "id3", Tick: 179999, SegmentID: -1},
		{Type: "emsg", Tick: 180000, SegmentID: -1}, // exactly on cut -> next segment
		{Type: "sei", Tick: 449999, SegmentID: -1},
	}
	p, err := Build(scanRes(450000, 180000, evs...))
	if err != nil {
		t.Fatal(err)
	}
	got := []int{p.Events[0].SegmentID, p.Events[1].SegmentID, p.Events[2].SegmentID, p.Events[3].SegmentID}
	want := []int{0, 0, 1, 2}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d segment %d want %d", i, got[i], want[i])
		}
	}
}

func TestEventBeyondEndFaults(t *testing.T) {
	evs := []scan.Event{{Type: "id3", Tick: 600000, SegmentID: -1}}
	_, err := Build(scanRes(450000, 180000, evs...))
	if fe := fault.Of(err); fe == nil || fe.Code != fault.CodeMalformedEvent || fe.Tick != 600000 {
		t.Fatalf("want malformed event fault, got %+v", fe)
	}
}

func TestNoDurationFaults(t *testing.T) {
	_, err := Build(&scan.Result{SegmentTicks: 180000})
	if fe := fault.Of(err); fe == nil || fe.Code != fault.CodeMissingTrack {
		t.Fatalf("want missing track fault, got %+v", fe)
	}
}

func TestSegmentBounds(t *testing.T) {
	p, _ := Build(scanRes(360000, 180000))
	s, e := p.SegmentBounds(1)
	if s != 180000 || e != 360000 {
		t.Fatalf("bounds %d %d", s, e)
	}
	if p.SegmentOf(359999) != 1 || p.SegmentOf(360000) != 1 {
		t.Fatal("SegmentOf boundary")
	}
}
