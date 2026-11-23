package repair

import (
	"testing"

	"archivecheck/internal/parse"
	"archivecheck/internal/testkit"
)

func buildFixture(t *testing.T) (*parse.Init, *Ordered) {
	t.Helper()
	initSpec, fragSpecs := testkit.StandardSampleLayout()
	m, err := testkit.Generate(t.TempDir(), initSpec, fragSpecs)
	if err != nil {
		t.Fatal(err)
	}
	init, err := parse.ParseInit(mustOpen(t, m.Segments[0].Path), "init.mp4")
	if err != nil {
		t.Fatal(err)
	}
	var frags []*parse.Fragment
	for _, s := range m.Segments[1:] {
		f, err := parse.ParseFragment(mustOpen(t, s.Path), s.Name, init)
		if err != nil {
			t.Fatal(err)
		}
		frags = append(frags, f)
	}
	ord, err := Order(frags)
	if err != nil {
		t.Fatal(err)
	}
	return init, ord
}

func TestTimelineRepairsBackwardTfdt(t *testing.T) {
	init, ord := buildFixture(t)
	if ord.Missing != 1 {
		t.Fatalf("missing = %d, want 1", ord.Missing)
	}
	tl, err := Build(ord, init)
	if err != nil {
		t.Fatal(err)
	}
	if tl.Repaired != 2 {
		t.Fatalf("repaired = %d, want 2", tl.Repaired)
	}
	if len(tl.Fragments) != 12 {
		t.Fatalf("timeline fragments = %d, want 12", len(tl.Fragments))
	}
	prevEnd := uint64(0)
	for i, f := range tl.Fragments {
		if f.RepairedBase < prevEnd {
			t.Fatalf("fragment %d begins at %d before previous end %d", i, f.RepairedBase, prevEnd)
		}
		for j, s := range f.Samples {
			if s.DTS < prevEnd && j > 0 {
				t.Fatalf("fragment %d sample %d DTS %d < %d", i, j, s.DTS, prevEnd)
			}
			prevEnd = s.EndDTS
		}
	}
	var repaired []FragmentTimeline
	for _, f := range tl.Fragments {
		if f.Reason == ReasonBackward {
			repaired = append(repaired, f)
		}
	}
	if len(repaired) != 2 {
		t.Fatalf("repaired fragments = %d", len(repaired))
	}
	if repaired[0].OriginalBase != 36000 || repaired[0].RepairedBase != 48000 {
		t.Fatalf("first repair = %+v", repaired[0])
	}
	if repaired[1].OriginalBase != 84000 || repaired[1].RepairedBase != 108000 {
		t.Fatalf("second repair = %+v", repaired[1])
	}
	for _, s := range repaired[0].Samples {
		if s.OriginalDTS >= s.DTS {
			t.Fatalf("original DTS %d not preserved against %d", s.OriginalDTS, s.DTS)
		}
	}
}

func TestTimelineRecordsForwardGap(t *testing.T) {
	init := &parse.Init{Segment: "init.mp4", Tracks: map[uint32]*parse.Track{
		1: {ID: 1, Timescale: 1000},
	}}
	ord := &Ordered{Fragments: []*parse.Fragment{
		{Segment: "a", Sequence: 1, HasMfhd: true,
			Trafs: []parse.Traf{{TrackID: 1, BaseDecodeTime: 0, Samples: []parse.Sample{{TrackID: 1, Duration: 100}}}}},
		{Segment: "b", Sequence: 2, HasMfhd: true,
			Trafs: []parse.Traf{{TrackID: 1, BaseDecodeTime: 500, Samples: []parse.Sample{{TrackID: 1, Duration: 100}}}}},
	}}
	tl, err := Build(ord, init)
	if err != nil {
		t.Fatal(err)
	}
	if tl.Forward != 1 || tl.Repaired != 0 {
		t.Fatalf("forward=%d repaired=%d", tl.Forward, tl.Repaired)
	}
	if tl.Fragments[1].Reason != ReasonGap || tl.Fragments[1].RepairedBase != 500 {
		t.Fatalf("entry = %+v", tl.Fragments[1])
	}
}
