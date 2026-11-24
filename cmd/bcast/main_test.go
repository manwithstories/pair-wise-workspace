package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureRun invokes the command in-process and returns the exit code.
func captureRun(t *testing.T, args ...string) int {
	t.Helper()
	return run(args)
}

func TestDemoThenVerifyExitsZero(t *testing.T) {
	dir := t.TempDir()
	cp := filepath.Join(dir, "cp.json")
	code := captureRun(t, "demo", "--seed", "17", "--segments", "12",
		"--damage", "truncate,broken-box,missing-index", "--checkpoint", cp)
	if code != 0 {
		t.Fatalf("demo exit=%d", code)
	}
	if _, err := os.Stat(cp); err != nil {
		t.Fatalf("checkpoint missing: %v", err)
	}
	if code := captureRun(t, "verify", "--checkpoint", cp); code != 0 {
		t.Fatalf("verify exit=%d", code)
	}
}

func TestBadDamageIsUsageError(t *testing.T) {
	dir := t.TempDir()
	code := captureRun(t, "demo", "--checkpoint", filepath.Join(dir, "cp.json"), "--damage", "nonsense")
	if code != 1 {
		t.Fatalf("exit=%d want 1", code)
	}
}

func TestMissingCheckpointFlagRejected(t *testing.T) {
	if code := captureRun(t, "demo"); code != 1 {
		t.Fatalf("exit=%d", code)
	}
	if code := captureRun(t, "verify"); code != 1 {
		t.Fatalf("exit=%d", code)
	}
}

func TestUnknownCommand(t *testing.T) {
	if code := captureRun(t, "frobnicate"); code != 1 {
		t.Fatalf("exit=%d", code)
	}
}

func TestCorruptCheckpointIsDiscarded(t *testing.T) {
	dir := t.TempDir()
	cp := filepath.Join(dir, "cp.json")
	if err := os.WriteFile(cp, []byte("{bad json"), 0o644); err != nil {
		t.Fatal(err)
	}
	code := captureRun(t, "demo", "--seed", "17", "--segments", "12", "--checkpoint", cp)
	if code != 0 {
		t.Fatalf("demo should recover from a corrupt checkpoint, exit=%d", code)
	}
	data, _ := os.ReadFile(cp)
	if !strings.Contains(string(data), "\"version\"") {
		t.Fatal("checkpoint not rewritten")
	}
}

func TestDefaultOutDir(t *testing.T) {
	if got := defaultOutDir("/tmp/x/cp.json"); got != "/tmp/x/cp-out" {
		t.Fatalf("got %q", got)
	}
	if got := defaultOutDir("/tmp/x/cp"); got != "/tmp/x/cp.out" {
		t.Fatalf("got %q", got)
	}
}
