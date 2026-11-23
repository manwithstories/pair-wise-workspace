package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"mediacore/internal/pipeline"
)

func TestRunDemoHealthy(t *testing.T) {
	dir := t.TempDir()
	code := run([]string{"demo", "--seed", "42", "--workers", "4",
		"--checkpoint", filepath.Join(dir, "c.json"),
		"--out", filepath.Join(dir, "o.mp4")})
	if code != exitOK {
		t.Fatalf("exit code %d, want 0", code)
	}
}

func TestRunDemoCancelled(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	code := run([]string{"demo", "--seed", "42", "--workers", "4",
		"--block-workers", "--cancel-after", "200ms",
		"--checkpoint", filepath.Join(dir, "c.json"),
		"--out", filepath.Join(dir, "o.mp4")})
	if code != exitOK {
		t.Fatalf("expected cancellation to be a clean exit 0, got %d", code)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("cancelled run did not exit within 3 seconds")
	}
}

func TestRunUsageErrors(t *testing.T) {
	if code := run(nil); code != exitUsageOrInternal {
		t.Fatalf("no args exit %d, want 2", code)
	}
	dir := t.TempDir()
	if code := run([]string{"demo", "--workers", "0",
		"--checkpoint", filepath.Join(dir, "c.json"),
		"--out", filepath.Join(dir, "o.mp4")}); code != exitUsageOrInternal {
		t.Fatalf("bad workers exit %d, want 2", code)
	}
}

func TestResultJSONShape(t *testing.T) {
	// The printed object must contain every acceptance field.
	r := pipeline.Result{Status: "ok", Tracks: 3, Samples: 61, FirstErrorOffset: -1,
		OffsetErrorBytes: 0, ResumeMS: 0, SHA256: "abc", OrderedEqual: true}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"status", "tracks", "samples", "first_error_offset",
		"offset_error_bytes", "resume_ms", "sha256", "ordered_equal"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("missing acceptance field %q in %s", key, b)
		}
	}
}
