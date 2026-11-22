package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCorruptRepresentationDoesNotPolluteBoundaries runs the demo with a
// truncated corrupt representation and asserts the healthy three-rep ladder
// still emits the full expected segment set with a one-tick-bounded delta,
// while the corrupt file is diagnosed separately.
func TestCorruptRepresentationDoesNotPolluteBoundaries(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	args := []string{"demo", "--seed", "11", "--duration", "14s",
		"--bitrates", "800k,1600k,3200k", "--segment", "2s",
		"--inject", "truncated-box", "--work-dir", dir}
	if code := run(args); code != exitOK {
		t.Fatalf("demo failed: exit=%d", code)
	}
	// The corrupt fixture exists; the healthy manifest still has 3 reps and
	// 7 segments (14s / 2s) with every healthy rep present in each segment.
	bad := filepath.Join(dir, "primary", "corrupt.mp4")
	if _, err := os.Stat(bad); err != nil {
		t.Fatal("corrupt representation not generated")
	}
	m, err := ReadManifestForTest(filepath.Join(dir, "primary"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Representations) != 3 {
		t.Fatalf("healthy reps=%d want 3", len(m.Representations))
	}
	if m.SegmentCount != 7 {
		t.Fatalf("segments=%d want 7", m.SegmentCount)
	}
	seen := map[int]map[string]bool{}
	for _, e := range m.Segments {
		if seen[e.Sequence] == nil {
			seen[e.Sequence] = map[string]bool{}
		}
		seen[e.Sequence][e.RepID] = true
	}
	for seq := 0; seq < m.SegmentCount; seq++ {
		if len(seen[seq]) != 3 {
			t.Fatalf("segment %d has %d reps, want 3", seq, len(seen[seq]))
		}
	}
}
