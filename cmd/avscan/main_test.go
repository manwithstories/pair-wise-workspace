package main

import (
	"os"
	"path/filepath"
	"testing"
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
