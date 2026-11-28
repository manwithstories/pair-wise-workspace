package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestDemoThenVerify runs the full acceptance flow on a small fixture set:
// demo generates cameras and prints passing metrics, verify re-audits.
func TestDemoThenVerify(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	out := filepath.Join(t.TempDir(), "demo")
	if code := run([]string{"demo", "--seed", "17", "--seconds", "6",
		"--segment", "2", "--targets", "400000,800000", "--out", out}); code != 0 {
		t.Fatalf("demo exit = %d, want 0", code)
	}
	if code := run([]string{"verify", "--dir", out}); code != 0 {
		t.Fatalf("verify exit = %d, want 0", code)
	}
	// Inputs must be untouched by the rerun: source tree still complete.
	for _, cam := range []string{"cam1", "cam2", "cam3"} {
		if _, err := findMedia(filepath.Join(out, "src", cam)); err != nil {
			t.Errorf("source missing after run: %v", err)
		}
	}
	rep, err := readReport(out)
	if err != nil {
		t.Fatal(err)
	}
	if rep.partial() {
		t.Error("all three cameras must be delivered")
	}
	if got := rep.recoveredSpans(); got < 2 {
		t.Errorf("recovered spans = %d, want >= 2", got)
	}
}

func TestParseTargets(t *testing.T) {
	got, err := parseTargets("400000, 800000")
	if err != nil || len(got) != 2 || got[1] != 800000 {
		t.Errorf("parseTargets = %v, %v", got, err)
	}
	if _, err := parseTargets("abc"); err == nil {
		t.Error("bad rate must fail")
	}
	if _, err := parseTargets(""); err == nil {
		t.Error("empty targets must fail")
	}
}

func TestUnknownCommandFails(t *testing.T) {
	if code := run([]string{"frobnicate"}); code != exitFailure {
		t.Errorf("exit = %d, want %d", code, exitFailure)
	}
}

func TestExpectedDurationRelative(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "timecode.txt")
	content := "fps=25\n23:59:59:00\n23:59:59:10\n00:00:00:00\n00:00:00:10\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	d, faults, err := expectedDuration(p)
	if err != nil {
		t.Fatal(err)
	}
	// Labels cross midnight: 1.4 s span plus one 0.4 s interval, relative
	// to the first label.
	if d.Millis() != 1800 {
		t.Errorf("expected duration = %v, want 1.8 s", d)
	}
	if len(faults.OfKind("timecode_wrap")) != 1 {
		t.Errorf("want wrap fault, got %v", faults)
	}
}
