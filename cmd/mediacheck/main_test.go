package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDemoArgsDefaults(t *testing.T) {
	d, err := parseDemoArgs([]string{"--seed", "42", "--duration", "7200s"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Seed != 42 || d.DurationSec != 7200 || d.SegmentSec != 6 {
		t.Fatalf("defaults wrong: %+v", d)
	}
	if d.CheckpointEvery != 256 || d.LossBurst != 12 || d.TruncateTail != 1 {
		t.Fatalf("damage defaults wrong: %+v", d)
	}
	if len(d.Representations) != 3 {
		t.Fatalf("representations = %v", d.Representations)
	}
}

func TestParseDemoArgsRejectsBad(t *testing.T) {
	bad := [][]string{
		{"--duration", "-5s"},
		{"--segment", "0s"},
		{"--checkpoint-every", "0"},
		{"--truncate-tail", "2"},
		{"--unknown", "1"},
		{"--seed"},
	}
	for _, args := range bad {
		if _, err := parseDemoArgs(args); err == nil {
			t.Fatalf("expected error for %v", args)
		}
	}
}

func TestRunExitCodes(t *testing.T) {
	if code := run([]string{"bogus"}); code != exitBadArgs {
		t.Fatalf("bogus subcommand exit = %d, want %d", code, exitBadArgs)
	}
	if code := run([]string{"demo", "--duration", "0s"}); code != exitBadArgs {
		t.Fatalf("bad duration exit = %d, want %d", code, exitBadArgs)
	}
}

func TestRunHealthyDemoEndToEnd(t *testing.T) {
	dir := t.TempDir()
	code := run([]string{
		"demo", "--seed", "42", "--duration", "36s",
		"--representations", "360p,720p,1080p",
		"--segment", "6s", "--checkpoint-every", "32",
		"--loss-burst", "12", "--truncate-tail", "1",
		"--work-dir", dir,
	})
	if code != exitOK {
		t.Fatalf("demo exit = %d, want %d", code, exitOK)
	}
}

func TestRunNoTruncationHealthy(t *testing.T) {
	dir := t.TempDir()
	code := run([]string{
		"demo", "--seed", "7", "--duration", "24s",
		"--representations", "360p,720p", "--segment", "6s",
		"--checkpoint-every", "16", "--loss-burst", "0",
		"--truncate-tail", "0", "--work-dir", dir,
	})
	if code != exitOK {
		t.Fatalf("healthy demo exit = %d", code)
	}
}

func TestSanitizeStripsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory available")
	}
	out := sanitize("failure at " + filepath.Join(home, "secret", "file.mp4"))
	if strings.Contains(out, home) {
		t.Fatalf("sensitive home path leaked: %s", out)
	}
	if !strings.Contains(out, "~") {
		t.Fatalf("expected home replaced with tilde: %s", out)
	}
}
