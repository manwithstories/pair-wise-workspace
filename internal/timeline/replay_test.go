package timeline

import (
	"testing"

	"example.com/medialab/archive/internal/model"
)

func TestSegmentIndexAndBoundary(t *testing.T) {
	if SegmentIndex(0, 10) != 0 || SegmentIndex(9_999_999, 10_000_000) != 0 || SegmentIndex(10_000_000, 10_000_000) != 1 {
		t.Fatal("segment index broken")
	}
	if Boundary(3, 10) != 30 {
		t.Fatalf("boundary = %d", Boundary(3, 10))
	}
	if SegmentIndex(-1, 10) != 0 {
		t.Fatal("negative time must clamp to zero")
	}
}

func TestAffectedSegments(t *testing.T) {
	got := AffectedSegments(9_000_000, 21_000_000, 10_000_000)
	want := []int{0, 1, 2}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	// Degenerate intervals still touch exactly one segment.
	if got := AffectedSegments(10, 10, 10); len(got) != 1 || got[0] != 1 {
		t.Fatalf("degenerate = %v", got)
	}
}

func TestVersionBookPromoteAndForget(t *testing.T) {
	b := NewVersionBook()
	rep := model.RepresentationID("64k")
	b.Record(rep, 0)
	b.Record(rep, 1)
	if b.Current(SegmentKey{Rep: rep, Index: 0}) != 1 {
		t.Fatal("record did not set v1")
	}
	r := b.Promote(rep, 0, 0, 10, model.PacketID{Stream: "s", Seq: 4}, 1234)
	if r.FromVersion != 1 || r.ToVersion != 2 || r.Rep != rep {
		t.Fatalf("replay = %+v", r)
	}
	if b.MaxVersion() != 2 || b.ReplayedSegments() != 1 {
		t.Fatalf("book = %+v", b)
	}
	b.Forget(rep, 1)
	if b.Current(SegmentKey{Rep: rep, Index: 0}) != 0 {
		t.Fatal("forget did not drop old slots")
	}
	if b.MaxVersion() != 2 || b.ReplayedSegments() != 1 {
		t.Fatal("lifetime counters must survive forget")
	}
}

func TestVersionBookExportImport(t *testing.T) {
	b := NewVersionBook()
	rep := model.RepresentationID("128k-hi")
	b.Promote(rep, 7, 0, 10, model.PacketID{Stream: "s", Seq: 1}, 0)
	m, maxVer, replayed := b.Export()
	var restored VersionBook
	if err := restored.Import(m, maxVer, replayed); err != nil {
		t.Fatal(err)
	}
	if restored.Current(SegmentKey{Rep: rep, Index: 7}) != 2 {
		t.Fatalf("restored version = %d", restored.Current(SegmentKey{Rep: rep, Index: 7}))
	}
	if restored.MaxVersion() != maxVer || restored.ReplayedSegments() != replayed {
		t.Fatal("counters not restored")
	}
	var bad VersionBook
	if err := bad.Import(map[string]int{"no-slash": 1}, 1, 0); err == nil {
		t.Fatal("malformed key accepted")
	}
}
