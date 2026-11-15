package segment

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanDedupLastWinsAndBadNames(t *testing.T) {
	dir := t.TempDir()
	files := map[string]int{
		"r0-s000002-t0000000008000-d4000.ts":     200000,
		"r0-s000000-t0000000000000-d4000.rt2.ts": 200001,
		"r0-s000001-t0000000004000-d4000.ts":     200000,
		"r0-s000000-t0000000000000-d4000.rt3.ts": 200002,
		"illegal-name.ts":                        10,
	}
	for name, size := range files {
		data := make([]byte, size)
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	result, err := ScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if result.DuplicateConflicts != 1 {
		t.Fatalf("duplicate conflicts = %d, want 1", result.DuplicateConflicts)
	}
	if len(result.Segments) != 3 {
		t.Fatalf("unique segments = %d, want 3", len(result.Segments))
	}
	// Files are sorted lexicographically: .rt3 sorts after .rt2 and must win.
	var seq0 Segment
	for _, seg := range result.Segments {
		if seg.Seq == 0 {
			seq0 = seg
		}
	}
	if seq0.FileName != "r0-s000000-t0000000000000-d4000.rt3.ts" || seq0.Size != 200002 {
		t.Fatalf("last-wins failed, got %+v", seq0)
	}
	if result.Segments[0].Seq != 0 || result.Segments[1].Seq != 1 || result.Segments[2].Seq != 2 {
		t.Fatalf("segments not reordered by seq: %+v", result.Segments)
	}
	if len(result.Failures) != 1 || result.Failures[0].Kind != "bad_name" {
		t.Fatalf("bad name not recorded: %+v", result.Failures)
	}
}
