package repair

import (
	"testing"

	"archivecheck/internal/parse"
	"archivecheck/internal/testkit"
)

func stdTrack(t *testing.T) ([]*parse.Fragment, *parse.Init, *parse.Track) {
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
	items := make([]Item, len(frags))
	for i, f := range frags {
		items[i] = Item{Segment: f.Segment, HasMfhd: f.HasMfhd, Sequence: f.Sequence, Index: i}
	}
	ord, err := Order(items)
	if err != nil {
		t.Fatal(err)
	}
	ordered := make([]*parse.Fragment, len(ord.Items))
	for i, it := range ord.Items {
		ordered[i] = frags[it.Index]
	}
	return ordered, init, init.Tracks[1]
}

// TestBuilderReportsCountsPerFragment asserts the incremental counters that the
// pipeline persists at each checkpoint boundary.
func TestBuilderReportsCountsPerFragment(t *testing.T) {
	frags, _, track := stdTrack(t)
	// Ordered fragments are indices 0..11 of the sorted order (sequence 1..6,
	// 8..13); the two regressions land at ordered positions 4 and 9.
	want := map[int]int{4: 1, 8: 1, 11: 2}
	b, err := NewBuilder(0, track)
	if err != nil {
		t.Fatal(err)
	}
	for i, f := range frags {
		if _, err := b.Add(f); err != nil {
			t.Fatal(err)
		}
		repaired, _ := b.Counts()
		if got, ok := want[i]; ok && repaired != got {
			t.Fatalf("after fragment %d repaired = %d, want %d", i, repaired, got)
		}
	}
	if repaired, _ := b.Counts(); repaired != 2 {
		t.Fatalf("final repaired = %d, want 2", repaired)
	}
}

func TestBuilderSeededResumeContinuesCounts(t *testing.T) {
	track := &parse.Track{ID: 1, Timescale: 1000}
	mk := func(seg string, base uint64) *parse.Fragment {
		return &parse.Fragment{Segment: seg, HasMfhd: true, Sequence: 1,
			Trafs: []parse.Traf{{TrackID: 1, BaseDecodeTime: base,
				Samples: []parse.Sample{{TrackID: 1, Duration: 100, Size: 1, Offset: 0}}}}}
	}
	b, err := NewBuilder(0, track)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Add(mk("a", 500)); err != nil {
		t.Fatal(err)
	}
	next, started := b.Next()
	if !started {
		t.Fatal("builder did not mark started")
	}
	b2, err := NewBuilder(0, track)
	if err != nil {
		t.Fatal(err)
	}
	b2.Seed(next, started)
	if _, err := b2.Add(mk("b", 500)); err != nil {
		t.Fatal(err)
	}
	if got, _ := b2.Counts(); got != 1 {
		t.Fatalf("seeded repaired = %d, want 1", got)
	}
}
