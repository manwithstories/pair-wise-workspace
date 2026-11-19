package index

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediapack/internal/manifest"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	cp := &Checkpoint{
		InputFingerprint: "abc", ConfigHash: "cfg", SamplesProcessed: 128,
		Segments: []manifest.SegmentEntry{{Index: 0, StartMs: 0, EndMs: 2000}},
	}
	if err := w.Save(cp); err != nil {
		t.Fatal(err)
	}
	if cp.Seq != 0 {
		t.Fatalf("seq: %d", cp.Seq)
	}
	cp2 := &Checkpoint{InputFingerprint: "abc", ConfigHash: "cfg", SamplesProcessed: 256}
	if err := w.Save(cp2); err != nil {
		t.Fatal(err)
	}
	if cp2.Seq != 1 {
		t.Fatalf("seq: %d", cp2.Seq)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SamplesProcessed != 256 || loaded.Seq != 1 {
		t.Fatalf("loaded: %+v", loaded)
	}
	// No tmp file may survive an atomic save.
	entries, _ := os.ReadDir(filepath.Join(dir, "index"))
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("stale tmp file: %s", e.Name())
		}
	}
}

func TestValidate(t *testing.T) {
	cp := &Checkpoint{InputFingerprint: "a", ConfigHash: "c"}
	if err := cp.Validate("a", "c"); err != nil {
		t.Fatal(err)
	}
	if err := cp.Validate("b", "c"); !errors.Is(err, ErrMismatch) {
		t.Fatalf("fingerprint mismatch: %v", err)
	}
	if err := cp.Validate("a", "d"); !errors.Is(err, ErrMismatch) {
		t.Fatalf("config mismatch: %v", err)
	}
}

func TestLoadMissing(t *testing.T) {
	if _, err := Load(t.TempDir()); !os.IsNotExist(err) {
		t.Fatalf("expected not-exist, got %v", err)
	}
}

// TestShortFinalBlock covers a checkpoint written for a final block that is
// smaller than the configured checkpoint interval: it must still persist.
func TestShortFinalBlock(t *testing.T) {
	dir := t.TempDir()
	w, _ := NewWriter(dir)
	// Simulate 1069 samples with interval 128: 8 full blocks + 45 tail.
	for processed := uint64(128); processed <= 1024; processed += 128 {
		if err := w.Save(&Checkpoint{SamplesProcessed: processed}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Save(&Checkpoint{SamplesProcessed: 1069}); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SamplesProcessed != 1069 {
		t.Fatalf("final short block lost: %d", loaded.SamplesProcessed)
	}
}
