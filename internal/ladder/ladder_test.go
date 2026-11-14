package ladder

import (
	"testing"

	"segmanifest/internal/manifest"
)

func TestNormalizeLadderSortsAndDeduplicates(t *testing.T) {
	rungs, err := NormalizeLadder([]Rung{{ID: "b", Bandwidth: 300}, {ID: "a", Bandwidth: 100}, {ID: "c", Bandwidth: 300}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rungs) != 2 || rungs[0].ID != "a" || rungs[1].ID != "b" {
		t.Fatalf("rungs=%+v", rungs)
	}
	if _, err := NormalizeLadder([]Rung{{ID: "x", Bandwidth: 1}, {ID: "x", Bandwidth: 2}}); err == nil {
		t.Fatal("expected duplicate ID error")
	}
}

func TestBuildGridUsesSharedPoints(t *testing.T) {
	rep := manifest.Representation{ID: "r", Bandwidth: 1, Segments: make([]manifest.Segment, 120)}
	for i := range rep.Segments {
		rep.Segments[i] = manifest.Segment{Number: uint64(i + 1), StartUS: int64(i) * 4_000_000, DurationUS: 4_000_000}
	}
	grid, err := BuildGrid([]manifest.Representation{rep}, 4_000_000, 2_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(grid.Points) != 121 {
		t.Fatalf("grid points=%d", len(grid.Points))
	}
	index, drift := grid.Nearest(4_001_500)
	if index != 1 || drift != 1_500 {
		t.Fatalf("nearest index=%d drift=%d", index, drift)
	}
}

func TestInvalidTolerance(t *testing.T) {
	for _, tolerance := range []int64{0, 2_000_001} {
		if err := ValidateConfig(4_000_000, tolerance); err == nil {
			t.Fatalf("tolerance %d should be rejected", tolerance)
		}
	}
}
