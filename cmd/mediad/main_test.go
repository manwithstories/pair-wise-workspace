package main

import (
	"path/filepath"
	"testing"
)

func TestRunDemoSuccessAndResume(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "work")
	code := run([]string{"demo", "--seed", "41", "--tracks", "3",
		"--resume", "--workdir", dir})
	if code != 0 {
		t.Fatalf("demo exit code = %d, want 0", code)
	}
}

func TestRunRejectsInvalidArgs(t *testing.T) {
	if code := run([]string{"demo", "--tracks", "9"}); code != 2 {
		t.Fatalf("bad tracks exit = %d, want 2", code)
	}
	if code := run([]string{"bogus"}); code != 2 {
		t.Fatalf("bad subcommand exit = %d, want 2", code)
	}
	if code := run(nil); code != 2 {
		t.Fatalf("no args exit = %d, want 2", code)
	}
	if code := run([]string{"help"}); code != 0 {
		t.Fatalf("help exit = %d, want 0", code)
	}
}

func TestRunRejectsPathEscape(t *testing.T) {
	code := run([]string{"demo", "--workdir", "../escape-dir"})
	if code != 2 {
		t.Fatalf("path escape exit = %d, want 2", code)
	}
}

func TestRunDeterministicAcrossCleanWorkdirs(t *testing.T) {
	d1 := filepath.Join(t.TempDir(), "a")
	d2 := filepath.Join(t.TempDir(), "b")
	if code := run([]string{"demo", "--seed", "7", "--tracks", "2",
		"--workdir", d1, "--tamper-cache=false"}); code != 0 {
		t.Fatalf("first clean run failed: %d", code)
	}
	if code := run([]string{"demo", "--seed", "7", "--tracks", "2",
		"--workdir", d2, "--tamper-cache=false"}); code != 0 {
		t.Fatalf("second clean run failed: %d", code)
	}
}
