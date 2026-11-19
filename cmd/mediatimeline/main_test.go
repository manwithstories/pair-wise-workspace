package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestDemoExitZero exercises the documented acceptance command through
// the real CLI entry point.
func TestDemoExitZero(t *testing.T) {
	out := filepath.Join(t.TempDir(), "frames")
	code := run([]string{"demo", "--seed", "41", "--drift-ppm", "85",
		"--wrap-at", "4294967000", "--out", out})
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(out, "manifest.json")); err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
}

func TestCheckModeDoesNotWrite(t *testing.T) {
	out := filepath.Join(t.TempDir(), "unused")
	if code := run([]string{"check", "--out", out}); code != 0 {
		t.Fatalf("check exit = %d", code)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("check mode created output: %v", err)
	}
}

func TestParameterErrorsExitTwo(t *testing.T) {
	cases := [][]string{
		{},
		{"bogus"},
		{"demo"}, // missing --out
		{"demo", "--out", "/tmp/x", "--frame-rate", "60"},
		{"demo", "--out", "/tmp/x", "--wrap-at", "99999999999"},
		{"demo", "--out", "/tmp/x", "--policy", "nonsense"},
		{"demo", "--out", "/tmp/x", "--window", "5:1"},
		{"demo", "--out", "/tmp/x", "--residual-ms", "0"},
		{"demo", "--out", "/tmp/x", "--unknown-flag"},
	}
	for _, args := range cases {
		if code := run(args); code != 2 {
			t.Errorf("run(%v) = %d, want 2", args, code)
		}
	}
}

func TestJSONSummaryShape(t *testing.T) {
	out := filepath.Join(t.TempDir(), "frames")
	if code := run([]string{"demo", "--out", out, "--json=false"}); code != 0 {
		t.Fatalf("human mode exit = %d", code)
	}
	raw, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["segments"].([]interface{}); !ok {
		t.Fatal("manifest has no segments array")
	}
}
