package timeline

import (
	"errors"
	"testing"

	"review/internal/ebml"
	"review/internal/fixture"
	"review/internal/media"
	"review/internal/mkv"
)

func decodeFixture(t *testing.T, d fixture.Document) *mkv.Document {
	t.Helper()
	doc, err := mkv.Decode(ebml.NewReader(byteReader(d)))
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return doc
}

func TestMergeInterleavesOnUnifiedTimeBase(t *testing.T) {
	docs := []*mkv.Document{
		decodeFixture(t, fixture.StudioInterview(42)),
		decodeFixture(t, fixture.LocationBRoll(42)),
	}
	tl, err := Merge(docs, DefaultPolicy())
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(tl.Blocks) == 0 {
		t.Fatal("merged timeline empty")
	}
	// Total deterministic order: time, then source id, then track number,
	// then source byte offset.
	less := func(a, b mkv.Block) bool {
		if a.Time != b.Time {
			return a.Time < b.Time
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.Track != b.Track {
			return a.Track < b.Track
		}
		return a.Offset < b.Offset
	}
	for i := 1; i < len(tl.Blocks); i++ {
		a, b := tl.Blocks[i-1], tl.Blocks[i]
		if less(b, a) {
			t.Fatalf("order violated at %d: %+v before %+v", i, a, b)
		}
	}
	// Stable, deterministic re-merge.
	tl2, err := Merge(docs, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(tl2.Blocks) != len(tl.Blocks) {
		t.Fatal("re-merge changed block count")
	}
	for i := range tl.Blocks {
		if tl.Blocks[i].Offset != tl2.Blocks[i].Offset ||
			tl.Blocks[i].Time != tl2.Blocks[i].Time {
			t.Fatal("re-merge is not stable/deterministic")
		}
	}
}

func TestNegativeTimestampRejected(t *testing.T) {
	doc := decodeFixture(t, fixture.StudioInterview(1))
	doc.Blocks[0].Time = media.FromNanoseconds(-1)
	_, err := Merge([]*mkv.Document{doc}, DefaultPolicy())
	var pe *media.PipelineError
	if !errors.As(err, &pe) || pe.Class != media.ClassBoundary {
		t.Fatalf("want boundary pipeline error, got %v", err)
	}
	if pe.BlockOff < 0 {
		t.Fatal("error must retain the offending block offset")
	}
}

func TestDuplicateTimestampPolicy(t *testing.T) {
	doc := decodeFixture(t, fixture.StudioInterview(2))
	// Force an exact same-track collision.
	doc.Blocks[1].Time = doc.Blocks[0].Time
	doc.Blocks[1].Track = doc.Blocks[0].Track

	if _, err := Merge([]*mkv.Document{doc}, DefaultPolicy()); err != nil {
		t.Fatalf("default policy should tie-break duplicates: %v", err)
	}
	strict := Policy{RejectNegative: true, RejectDuplicateTS: true}
	_, err := Merge([]*mkv.Document{doc}, strict)
	var pe *media.PipelineError
	if !errors.As(err, &pe) || pe.Class != media.ClassBoundary {
		t.Fatalf("strict policy must reject duplicate TS, got %v", err)
	}
}

func TestColourConflictFixedPriority(t *testing.T) {
	docs := []*mkv.Document{
		decodeFixture(t, fixture.ColourClash(42)),
		decodeFixture(t, fixture.LocationBRoll(42)),
	}
	tl, err := Merge(docs, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.ColourConflicts) == 0 {
		t.Fatal("colour conflicts were not recorded")
	}
	// Every conflict must name both sources and keep distinct values.
	for _, c := range tl.ColourConflicts {
		if c.WinnerTrack == 0 || c.LoserTrack == 0 {
			t.Fatalf("conflict missing source provenance: %+v", c)
		}
		if c.WinnerValue == c.LoserValue {
			t.Fatalf("conflict recorded for equal values: %+v", c)
		}
	}
	// The earliest document's track 1 (matrix=9) must win over track 2
	// (matrix=1) because document order outranks track number.
	winner := tl.ColourWinners["matrix"]
	if winner.Matrix != 9 || winner.Track != 1 {
		t.Fatalf("matrix winner = %+v, want track 1 value 9", winner)
	}
}

func TestMissingColourNeverFabricated(t *testing.T) {
	doc := decodeFixture(t, fixture.RemoteNarration(3))
	tl, err := Merge([]*mkv.Document{doc}, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"matrix", "transfer", "primaries"} {
		if _, ok := tl.ColourWinners[field]; ok {
			t.Fatalf("absent colour field %q must not produce a winner", field)
		}
	}
}

func TestTrackCountAccumulates(t *testing.T) {
	docs := []*mkv.Document{
		decodeFixture(t, fixture.StudioInterview(4)),
		decodeFixture(t, fixture.RemoteNarration(4)),
	}
	tl, err := Merge(docs, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Tracks) != len(docs[0].Tracks)+len(docs[1].Tracks) {
		t.Fatalf("track count = %d", len(tl.Tracks))
	}
}
