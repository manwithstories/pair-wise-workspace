package main

import (
	"context"
	"path/filepath"
	"testing"
)

// TestExitCodeSuccess runs the full demo and expects exit 0.
func TestExitCodeSuccess(t *testing.T) {
	out := filepath.Join(t.TempDir(), "run")
	code := run(context.Background(), []string{
		"demo", "--tracks", "2", "--packets", "2000", "--reorder-window", "64",
		"--chunk-packets", "256", "--queue", "64", "--slow-writer-ms", "0",
		"--out", out,
	})
	if code != exitOK {
		t.Fatalf("demo exit = %d, want %d", code, exitOK)
	}
	if code2 := run(context.Background(), []string{"verify", "--out", out}); code2 != exitOK {
		t.Fatalf("verify exit = %d, want %d", code2, exitOK)
	}
}

// TestExitCodeInvalidParam expects code 3 for bad parameters.
func TestExitCodeInvalidParam(t *testing.T) {
	out := filepath.Join(t.TempDir(), "run")
	code := run(context.Background(), []string{
		"demo", "--tracks", "0", "--packets", "100", "--out", out,
	})
	if code != exitParam {
		t.Fatalf("exit = %d, want param code %d", code, exitParam)
	}
}

// TestExitCodeMissingOut expects code 3 when --out is omitted.
func TestExitCodeMissingOut(t *testing.T) {
	code := run(context.Background(), []string{"demo", "--packets", "100"})
	if code != exitParam {
		t.Fatalf("exit = %d, want %d", code, exitParam)
	}
}

// TestExitCodeCorruptVerify expects code 2 when verify reads a tampered dir.
func TestExitCodeCorruptVerify(t *testing.T) {
	out := filepath.Join(t.TempDir(), "run")
	if code := run(context.Background(), []string{
		"demo", "--tracks", "2", "--packets", "1000", "--chunk-packets", "128",
		"--reorder-window", "16", "--queue", "32", "--slow-writer-ms", "0", "--out", out,
	}); code != exitOK {
		t.Fatalf("setup demo failed: %d", code)
	}
	// Corrupt a segment so re-hashing disagrees with the report.
	seg := filepath.Join(out, "segments", "segment_0000.m4s")
	writeTamper(t, seg)
	if code := run(context.Background(), []string{"verify", "--out", out}); code != exitCorrupt {
		t.Fatalf("tampered verify exit = %d, want %d", code, exitCorrupt)
	}
}

// TestExitCodeUnknownCommand expects code 3.
func TestExitCodeUnknownCommand(t *testing.T) {
	if code := run(context.Background(), []string{"frobnicate"}); code != exitParam {
		t.Fatalf("exit = %d, want %d", code, exitParam)
	}
}
