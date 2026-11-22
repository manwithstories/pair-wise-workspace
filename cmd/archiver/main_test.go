package main

import (
	"path/filepath"
	"testing"
)

// TestDemoExitZero runs the full demo path in-process and asserts the exit
// contract, keeping the acceptance demonstration covered by `go test`.
func TestDemoExitZero(t *testing.T) {
	dir := t.TempDir()
	code := run([]string{
		"demo", "--seed", "41", "--interrupt-after", "120", "--resume",
		"--loss", "0.03", "--reorder", "0.02", "--out", filepath.Join(dir, "demo"),
	})
	if code != 0 {
		t.Fatalf("demo exit = %d, want 0", code)
	}
}

func TestUnknownCommandExitsTwo(t *testing.T) {
	if code := run([]string{"bogus"}); code != 2 {
		t.Fatalf("exit = %d want 2", code)
	}
}

func TestHelpExitsZero(t *testing.T) {
	if code := run([]string{"help"}); code != 0 {
		t.Fatalf("exit = %d want 0", code)
	}
}
