package fixture

import (
	"os"
	"path/filepath"
	"testing"

	"mediapack/internal/demux"
	"mediapack/internal/fault"
)

// TestZeroDurationTrack builds a file whose video track has no samples and
// confirms the demuxer handles it without crashing or fatal faults.
func TestZeroDurationTrack(t *testing.T) {
	dir := t.TempDir()
	tracks := []trackSpec{
		{id: 1, kind: "video", timescale: videoTimescale, width: 640, height: 360},
		audioTrack(3),
	}
	path := filepath.Join(dir, "zerodur.mov")
	if err := os.WriteFile(path, buildMP4(1000, tracks, false), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := demux.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Faults.MaxSeverity() >= fault.Fatal {
		t.Fatalf("zero-duration track must not be fatal: %v", c.Faults.Faults)
	}
	if len(c.Tracks) != 2 {
		t.Fatalf("tracks: %d", len(c.Tracks))
	}
	if len(c.Tracks[0].Samples) != 0 {
		t.Fatalf("samples: %d", len(c.Tracks[0].Samples))
	}
	if c.Tracks[0].Duration() != 0 {
		t.Fatal("duration must be zero")
	}
}

// TestGeneratedFilesAreStable pins deterministic output: two generations
// must produce byte-identical files.
func TestGeneratedFilesAreStable(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()
	if err := Generate(d1); err != nil {
		t.Fatal(err)
	}
	if err := Generate(d2); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"valid.mov", "truncated.mov", "noindex.mov", "corrupt.mov"} {
		a, err := os.ReadFile(filepath.Join(d1, name))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(d2, name))
		if err != nil {
			t.Fatal(err)
		}
		if len(a) != len(b) {
			t.Fatalf("%s: length differs", name)
		}
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("%s: byte %d differs", name, i)
			}
		}
	}
}

// TestTruncatedIsPrefix ensures the truncated fixture really is a prefix of
// the valid one, so the moov box survives and only mdat is cut.
func TestTruncatedIsPrefix(t *testing.T) {
	dir := t.TempDir()
	if err := Generate(dir); err != nil {
		t.Fatal(err)
	}
	valid, _ := os.ReadFile(filepath.Join(dir, "valid.mov"))
	trunc, _ := os.ReadFile(filepath.Join(dir, "truncated.mov"))
	if len(trunc) >= len(valid) {
		t.Fatal("truncated must be shorter")
	}
	for i := range trunc {
		if trunc[i] != valid[i] {
			t.Fatal("truncated is not a prefix of valid")
		}
	}
}
