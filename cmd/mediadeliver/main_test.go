package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// checkpointFile mirrors the on-disk state JSON for test mutation.
type checkpointFile struct {
	Version int `json:"version"`
	Input   struct {
		ManifestSHA256 string            `json:"ManifestSHA256"`
		SourceSHA256   map[string]string `json:"SourceSHA256"`
	} `json:"input"`
	Completed map[string]any `json:"completed"`
	Order     []string       `json:"order"`
}

// TestRunReturnsThreeOnConflict proves a stale checkpoint whose input digest
// no longer matches maps to exit code 3 (processing/verification failure).
func TestRunReturnsThreeOnConflict(t *testing.T) {
	cp := filepath.Join(t.TempDir(), "cp.json")
	// A clean run must legitimately pass: enable the transient write fault so
	// recovery contributes the required recovered segment.
	code := run([]string{"demo", "--checkpoint", cp, "--seed", "42",
		"--queue", "8", "--slow-consumer-ms", "0", "--corrupt-every", "7"})
	if code != 0 {
		t.Fatalf("clean run exit=%d, want 0", code)
	}

	// Tamper with the recorded manifest digest in the checkpoint JSON.
	raw, err := os.ReadFile(cp)
	if err != nil {
		t.Fatal(err)
	}
	var st checkpointFile
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	st.Input.ManifestSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	mutated, err := json.MarshalIndent(&st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cp, mutated, 0o644); err != nil {
		t.Fatal(err)
	}

	code = run([]string{"demo", "--checkpoint", cp, "--seed", "42",
		"--queue", "8", "--slow-consumer-ms", "0", "--corrupt-every", "0"})
	if code != 3 {
		t.Fatalf("digest conflict exit=%d, want 3", code)
	}
}

// TestRunReturnsTwoOnBadArgs covers the invalid-parameter exit code.
func TestRunReturnsTwoOnBadArgs(t *testing.T) {
	if code := run(nil); code != 2 {
		t.Fatalf("no args exit=%d, want 2", code)
	}
	if code := run([]string{"nope"}); code != 2 {
		t.Fatalf("bad subcommand exit=%d, want 2", code)
	}
	if code := run([]string{"demo", "--queue", "-1"}); code != 2 {
		t.Fatalf("bad queue exit=%d, want 2", code)
	}
}
