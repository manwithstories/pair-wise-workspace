package segment

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPlanCoalescesVersionAndCCCluster(t *testing.T) {
	boundaries := []Boundary{
		{Offset: 300, Kind: "version", SnapshotID: 5},
		{Offset: 310, Kind: "cc_break", SnapshotID: 5},
		{Offset: 700, Kind: "cc_break", SnapshotID: 7},
	}
	segments := Plan(boundaries, 1000, 100, 2)
	if len(segments) != 3 {
		t.Fatalf("segments = %d: %+v", len(segments), segments)
	}
	if segments[0].StartByte != 0 || segments[0].EndByte != 300 || segments[0].Boundary != "version" {
		t.Fatalf("unexpected first segment: %+v", segments[0])
	}
	if segments[1].SnapshotID != 5 || segments[2].EndByte != 1000 || segments[2].Bytes != 300 {
		t.Fatalf("unexpected later segments: %+v", segments)
	}
}

func TestPlanEnforcesMinimumButAllowsShortTail(t *testing.T) {
	boundaries := []Boundary{
		{Offset: 50, Kind: "version", SnapshotID: 1},
		{Offset: 500, Kind: "version", SnapshotID: 2},
	}
	segments := Plan(boundaries, 600, 100, 0)
	if len(segments) != 2 {
		t.Fatalf("segments = %d: %+v", len(segments), segments)
	}
	if segments[1].StartByte != 500 || segments[1].Bytes != 100 {
		t.Fatalf("minimum/tail handling incorrect: %+v", segments)
	}
}

func TestWriteCopiesByteRangesAndManifest(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "in.ts")
	if err := os.WriteFile(input, []byte("abcdefghij"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{MinBytes: 4, Segments: []Segment{
		{Index: 0, StartByte: 0, EndByte: 4, Bytes: 4},
		{Index: 1, StartByte: 4, EndByte: 10, Bytes: 6},
	}}
	outDir := filepath.Join(dir, "segments")
	if err := Write(input, outDir, manifest); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"abcd", "efghij"} {
		got, err := os.ReadFile(filepath.Join(outDir, "segment-00"+string(rune('0'+i))+".ts"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("segment %d = %q, want %q", i, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(outDir, "manifest.json")); err != nil {
		t.Fatal(err)
	}
}
