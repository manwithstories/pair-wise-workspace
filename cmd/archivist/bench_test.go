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
