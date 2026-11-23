package repair

import (
	"testing"

	"archivecheck/internal/parse"
	"archivecheck/internal/testkit"
)

// TestBuilderReportsCountsPerFragment asserts the incremental counters that the
// pipeline persists at each checkpoint boundary, including the exact fragment
// at which each backward tfdt repair is applied.
func TestBuilderReportsCountsPerFragment(t *testing.T) {
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
	items := make([]Item, len(frags))
	for i, f := range frags {
		items[i] = Item{Segment: f.Segment, HasMfhd: f.HasMfhd, Sequence: f.Sequence, Index: i}
	}
	ord, err := Order(items)
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint32]int{
		5:  1, // frag-005 repairs the regression introduced by frag-005's tfdt
		6:  1,
		8:  1,
		9:  1,
		10: 1,
		11: 2, // frag-011 repairs the second regression
		12: 2,
		13: 2,
	}
	b, err := NewBuilder(init)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range ord.Items {
		if _, err := b.Add(frags[it.Index]); err != nil {
			t.Fatal(err)
		}
		repaired, _ := b.Counts()
		if got, ok := want[it.Sequence]; ok && repaired != got {
			t.Fatalf("after sequence %d repaired = %d, want %d", it.Sequence, repaired, got)
		}
	}
	if repaired, _ := b.Counts(); repaired != 2 {
		t.Fatalf("final repaired = %d, want 2", repaired)
	}
}

func TestBuilderSeededResumeContinuesCounts(t *testing.T) {
	init := &parse.Init{Segment: "init.mp4", Tracks: map[uint32]*parse.Track{
		1: {ID: 1, Timescale: 1000},
	}}
	mk := func(seg string, base uint64) *parse.Fragment {
		return &parse.Fragment{Segment: seg, HasMfhd: true, Sequence: 1,
			Trafs: []parse.Traf{{TrackID: 1, BaseDecodeTime: base,
				Samples: []parse.Sample{{TrackID: 1, Duration: 100, Size: 1, Offset: 0}}}}}
	}
	b, err := NewBuilder(init)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Add(mk("a", 500)); err != nil {
		t.Fatal(err)
	}
	repaired, _ := b.Counts()
	if repaired != 0 {
		t.Fatalf("repaired = %d", repaired)
	}
	// A regression against seeded state must be repaired once.
	b2, err := NewBuilder(init)
	if err != nil {
		t.Fatal(err)
	}
	b2.Seed(b.Next())
	if _, err := b2.Add(mk("b", 500)); err != nil {
		t.Fatal(err)
	}
	if got, _ := b2.Counts(); got != 1 {
		t.Fatalf("seeded repaired = %d, want 1", got)
	}
}
