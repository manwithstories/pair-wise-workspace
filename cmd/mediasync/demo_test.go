package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"mediasync/internal/render"
)

// TestCorruptModeReportsIncompleteTrack is the end-to-end fault-propagation
// guard: corrupt mode must surface unrecoverable gaps all the way to the
// exported manifest and mark the owning track incomplete, while normal mode
// stays clean.
func TestCorruptModeReportsIncompleteTrack(t *testing.T) {
	corruptDir := t.TempDir()
	if err := runDemo([]string{
		"--seed", "41", "--duration", "3s", "--loss-burst", "80ms",
		"--bandwidth", "2M,700k,3M", "--output", filepath.Join(corruptDir, "out"),
	}, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(corruptDir, "out", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m render.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Incomplete) == 0 {
		t.Fatal("corrupt run must report at least one incomplete track")
	}
	gapFrames := 0
	for _, f := range m.Frames {
		if f.Incomplete {
			gapFrames++
			if f.Conclusion != "incomplete_gap_placeholder" {
				t.Fatalf("gap frame conclusion wrong: %q", f.Conclusion)
			}
		}
	}
	if gapFrames == 0 {
		t.Fatal("corrupt manifest must contain incomplete gap-placeholder frames")
	}

	// The same inputs in normal mode must produce no incomplete tracks.
	normalDir := t.TempDir()
	if err := runDemo([]string{
		"--seed", "41", "--duration", "3s", "--loss-burst", "80ms",
		"--bandwidth", "2M,700k,3M", "--output", filepath.Join(normalDir, "out"),
	}, false); err != nil {
		t.Fatal(err)
	}
	raw2, err := os.ReadFile(filepath.Join(normalDir, "out", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var n render.Manifest
	if err := json.Unmarshal(raw2, &n); err != nil {
		t.Fatal(err)
	}
	if len(n.Incomplete) != 0 {
		t.Fatalf("normal run must have no incomplete tracks, got %v", n.Incomplete)
	}
	if len(n.Mismatches) != 0 {
		t.Fatalf("normal run must have zero boundary mismatches, got %d", len(n.Mismatches))
	}
}

// TestPathTraversalRejectedAtEntry ensures the raw --output path is rejected
// before any directory creation happens.
func TestPathTraversalRejectedAtEntry(t *testing.T) {
	base := t.TempDir()
	bad := base + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "escape"
	err := runDemo([]string{
		"--seed", "1", "--duration", "2s", "--loss-burst", "10ms",
		"--bandwidth", "1M", "--output", bad,
	}, false)
	if err == nil {
		t.Fatal("traversal output path must be rejected")
	}
	if _, statErr := os.Stat(filepath.Join(base, "escape")); !os.IsNotExist(statErr) {
		t.Fatal("rejected path must not create a directory")
	}
}
