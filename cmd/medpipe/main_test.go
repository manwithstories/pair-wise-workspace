package main

import "testing"

func TestRunExitCodes(t *testing.T) {
	if code := run(nil); code != exitUsage {
		t.Fatalf("empty args exit=%d want %d", code, exitUsage)
	}
	if code := run([]string{"frobnicate"}); code != exitUsage {
		t.Fatalf("bad command exit=%d want %d", code, exitUsage)
	}
	if code := run([]string{"help"}); code != exitOK {
		t.Fatalf("help exit=%d want %d", code, exitOK)
	}
}

func TestRunDemoHappyAndCorrupt(t *testing.T) {
	if code := run([]string{"demo", "--seed", "42", "--loss-burst", "3",
		"--latency-ms", "80", "--frames", "240"}); code != exitOK {
		t.Fatalf("healthy demo exit=%d want 0", code)
	}
	// A second identical run must reuse the stored index and still pass.
	if code := run([]string{"demo", "--seed", "42", "--loss-burst", "3",
		"--latency-ms", "80", "--frames", "240"}); code != exitOK {
		t.Fatalf("repeat demo exit=%d want 0", code)
	}
}

func TestRunDemoCorruptionFailsWithCode1(t *testing.T) {
	code := run([]string{"demo", "--seed", "42", "--corrupt-frame", "117",
		"--truncate-bytes", "9", "--index-incomplete"})
	if code != exitCorrupt {
		t.Fatalf("corrupt demo exit=%d want 1", code)
	}
}

func TestRunDemoBadFlag(t *testing.T) {
	if code := run([]string{"demo", "--frames", "0"}); code != exitUsage {
		t.Fatalf("bad flag exit=%d want 2", code)
	}
}
