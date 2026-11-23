package main

import "testing"

func TestParseDemo(t *testing.T) {
	a, err := parseDemo([]string{
		"--seed", "7", "--tracks", "3", "--frames", "500",
		"--corrupt-at", "120", "--checkpoint", "/tmp/cp.json",
		"--cancel-after", "200", "--resume",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.seed != 7 || a.tracks != 3 || a.frames != 500 || a.corruptAt != 120 {
		t.Fatalf("parsed values wrong: %+v", a)
	}
	if a.cancelAfter != 200 || !a.resume || a.checkpoint != "/tmp/cp.json" {
		t.Fatalf("flags wrong: %+v", a)
	}
}

func TestParseDemoDefaults(t *testing.T) {
	a, err := parseDemo([]string{"--checkpoint", "/tmp/c.json"})
	if err != nil {
		t.Fatal(err)
	}
	if a.seed != 42 || a.tracks != 4 || a.frames != 1200 || a.corruptAt != -1 {
		t.Fatalf("defaults wrong: %+v", a)
	}
	if a.cancelAfter != 0 || a.resume {
		t.Fatalf("default control flags wrong: %+v", a)
	}
}

func TestParseDemoRejectsEmptyCheckpoint(t *testing.T) {
	if _, err := parseDemo([]string{"--checkpoint", ""}); err == nil {
		t.Fatal("empty checkpoint path must be rejected")
	}
	// Unknown flag must surface a parse error rather than panic.
	if _, err := parseDemo([]string{"--bogus", "1"}); err == nil {
		t.Fatal("unknown flag must error")
	}
}

func TestStringHelpers(t *testing.T) {
	if !isCheckpointError(errCheckpoint()) {
		t.Fatal("checkpoint error not recognized")
	}
	if isCheckpointError(nil) {
		t.Fatal("nil must not be a checkpoint error")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func errCheckpoint() error { return errString("exit 5: mismatch") }
