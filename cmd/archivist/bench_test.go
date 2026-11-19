package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"archivearchivist/internal/testkit"
)

func TestBenchClampsIterations(t *testing.T) {
	dir := t.TempDir()
	fx, err := testkit.Generate(testkit.Config{OutDir: dir, VideoFrames: 60})
	if err != nil {
		t.Fatal(err)
	}
	jsonPath := filepath.Join(dir, "bench.json")
	code, out, errs := runCLI(t, "bench", "--input", fx.Timeline, "--capture", fx.Capture,
		"--iterations", "3", "--json", jsonPath)
	if code != 0 {
		t.Fatalf("bench exit %d: %s\n%s", code, errs, out)
	}
	if !strings.Contains(errs, "raised to the required minimum of 30") {
		t.Fatalf("bench did not report clamping: %s", errs)
	}
	b, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Bench struct {
			Iterations int `json:"iterations"`
		} `json:"bench"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Bench.Iterations < 30 {
		t.Fatalf("iterations = %d, want >= 30", doc.Bench.Iterations)
	}
}

func TestGenerateRejectsTinyFixtures(t *testing.T) {
	code, _, errs := runCLI(t, "generate", "--out", t.TempDir(), "--video-frames", "5")
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(errs, "at least 12") {
		t.Fatalf("unexpected error: %s", errs)
	}
}

func TestJSONCannotOverwriteInputs(t *testing.T) {
	dir := t.TempDir()
	fx, err := testkit.Generate(testkit.Config{OutDir: dir, VideoFrames: 30})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(fx.Timeline)
	if err != nil {
		t.Fatal(err)
	}
	code, _, errs := runCLI(t, "inspect", "--input", fx.Timeline, "--capture", fx.Capture,
		"--json", fx.Timeline)
	if code != 2 {
		t.Fatalf("exit %d, want 2 (%s)", code, errs)
	}
	after, err := os.ReadFile(fx.Timeline)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("--json overwrote the input timeline")
	}
	code, _, _ = runCLI(t, "recover", "--input", fx.Timeline, "--capture", fx.Capture,
		"--json", fx.Capture)
	if code != 2 {
		t.Fatalf("capture overwrite exit %d, want 2", code)
	}
}
