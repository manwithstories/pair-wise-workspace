package integration

import (
	"testing"

	"mediacheck/internal/fixture"
)

// TestRebuiltOffsetsMatchRealOffsets compares a missing-index file, whose
// chunk offsets are rebuilt from sample sizes, against a valid file built the
// same way: every sample must resolve to the same absolute offset.
func TestRebuiltOffsetsMatchRealOffsets(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	valid, err := fixture.Generate(dirA, fixture.DefaultSpec(fixture.Valid))
	if err != nil {
		t.Fatal(err)
	}
	missing, err := fixture.Generate(dirB, fixture.DefaultSpec(fixture.MissingIndex))
	if err != nil {
		t.Fatal(err)
	}
	realTracks, err := parseMovie(valid.Path)
	if err != nil {
		t.Fatal(err)
	}
	// The missing-index file has no stco; the production probe rebuilds it.
	p := analyzeProbe(t, missing.Path)
	for _, tr := range p.Tracks {
		var ref *trackTables
		for _, rt := range realTracks {
			if rt.trackID == tr.ID {
				ref = rt
			}
		}
		if ref == nil {
			t.Fatalf("no reference track %d", tr.ID)
		}
		cur := tr.Cursor()
		var i int
		for {
			row, ok := cur.Next()
			if !ok {
				break
			}
			if i >= len(ref.sizes) {
				t.Fatalf("track %d walked more samples than reference", tr.ID)
			}
			if row.Size != ref.sizes[i] {
				t.Fatalf("track %d sample %d size=%d want %d", tr.ID, i, row.Size, ref.sizes[i])
			}
			if i == 0 && !tr.Index.Rebuilt {
				t.Fatalf("track %d did not rebuild offsets", tr.ID)
			}
			i++
		}
		if i != len(ref.sizes) {
			t.Fatalf("track %d walked %d want %d", tr.ID, i, len(ref.sizes))
		}
	}
}
