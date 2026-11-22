package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"avscan/internal/batch"
)

func TestDemoEndToEndAndResume(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "nested", "index.json")

	code := runDemo([]string{"--out", out, "--block-samples", "64"})
	if code != 0 {
		t.Fatalf("first demo exit code = %d, want 0", code)
	}
	first, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("index not written: %v", err)
	}
	// Checkpoint must sit beside the index.
	if _, err := os.Stat(out + ".checkpoint"); err != nil {
		t.Fatalf("checkpoint missing: %v", err)
	}
	// Fixture inputs are reused from the stable sibling directory.
	if _, err := os.Stat(filepath.Join(filepath.Dir(out), ".avscan-fixtures", "healthy.mp4")); err != nil {
		t.Fatalf("stable fixture missing: %v", err)
	}

	code = runDemo([]string{"--out", out, "--block-samples", "64", "--resume"})
	if code != 0 {
		t.Fatalf("resume demo exit code = %d, want 0", code)
	}
	second, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("index missing after resume: %v", err)
	}
	if string(first) != string(second) {
		t.Error("resumed index bytes differ from the first run")
	}
	var rec batch.IndexRecord
	if err := json.Unmarshal(first, &rec); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, f := range rec.Faults {
		kinds[string(f.Kind)] = true
	}
	for _, want := range []string{"sample_offset_out_of_range", "rtp_sequence_gap", "missing_sync_samples"} {
		if !kinds[want] {
			t.Errorf("index faults missing kind %q; got %v", want, kinds)
		}
	}
	if len(rec.DamageSegments) < 2 {
		t.Errorf("damage segments = %d, want >= 2", len(rec.DamageSegments))
	}
}

func TestDemoRejectsMissingOut(t *testing.T) {
	if code := runDemo([]string{"--block-samples", "8"}); code != 2 {
		t.Errorf("missing --out exit = %d, want 2", code)
	}
}

func TestDemoRejectsBadBlockSize(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "i.json")
	if code := runDemo([]string{"--out", out, "--block-samples", "0"}); code != 2 {
		t.Errorf("bad block size exit = %d, want 2", code)
	}
}
