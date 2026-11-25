package main

import (
	"testing"

	"medpipe/internal/clock"
)

func TestRunHelpExitZero(t *testing.T) {
	if code := run([]string{"help"}); code != 0 {
		t.Fatalf("help exit = %d, want 0", code)
	}
	if code := run([]string{"--help"}); code != 0 {
		t.Fatalf("--help exit = %d, want 0", code)
	}
}

func TestRunNoArgsIsArgumentError(t *testing.T) {
	if code := run(nil); code != 2 {
		t.Fatalf("no-args exit = %d, want 2", code)
	}
}

func TestRunUnknownCommandIsArgumentError(t *testing.T) {
	if code := run([]string{"frobnicate"}); code != 2 {
		t.Fatalf("unknown command exit = %d, want 2", code)
	}
}

func TestRunSelfTestMissingDir(t *testing.T) {
	if code := run([]string{"selftest", "--seed", "73"}); code != 2 {
		t.Fatalf("missing --dir exit = %d, want 2", code)
	}
}

func TestRunSelfTestBadSeed(t *testing.T) {
	if code := run([]string{"selftest", "--dir", t.TempDir(), "--seed", "nan"}); code != 2 {
		t.Fatalf("bad --seed exit = %d, want 2", code)
	}
}

// TestExitCodeReachability proves the adapter's mapping covers the media
// corruption (3) and inconsistency (4) codes the internal packages emit.
func TestExitCodeReachability(t *testing.T) {
	cases := map[clock.ErrorKind]int{
		clock.KindCorrupt:      3,
		clock.KindUnsupported:  3,
		clock.KindInconsistent: 4,
	}
	for kind, want := range cases {
		if got := clock.ExitCode(clock.E(kind, "test")); got != want {
			t.Fatalf("kind %d -> exit %d, want %d", kind, got, want)
		}
	}
}
