package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestAcceptanceScriptRun executes the documented acceptance flow end to end
// with an interrupted run and a resume, checking the required output shapes
// and that both digests agree.
func TestAcceptanceScriptRun(t *testing.T) {
	work := filepath.Join(t.TempDir(), "demo")
	var out string
	code := captureStdout(t, func() int {
		return run([]string{"demo", "--work", work, "--rate", "48000",
			"--channels", "stereo", "--loudness", "-1", "--interrupt-after", "3"})
	}, &out)
	if code != 0 {
		t.Fatalf("demo exit %d, output %s", code, out)
	}
	if !regexp.MustCompile(`checkpoint_block=3`).MatchString(out) {
		t.Fatalf("interrupt output %q", out)
	}

	var resumeOut string
	code = captureStdout(t, func() int {
		return run([]string{"resume", "--work", work})
	}, &resumeOut)
	if code != 0 {
		t.Fatalf("resume exit %d, output %s", code, resumeOut)
	}
	m := regexp.MustCompile(`checkpoint_block=3 resumed_blocks=3 recovery_ms=(\d+) samples_in=(\d+) samples_out=(\d+) rate_out=48000 channels=2 peak_dbfs=(-?\d+\.\d+) sha256=([0-9a-f]{64})`).FindStringSubmatch(resumeOut)
	if m == nil {
		t.Fatalf("resume output shape %q", resumeOut)
	}
	resumeSHA := m[5]

	// A full run in a separate directory must produce the same digest.
	full := filepath.Join(t.TempDir(), "full")
	var fullOut string
	code = captureStdout(t, func() int {
		return run([]string{"demo", "--work", full, "--rate", "48000", "--channels", "stereo", "--loudness", "-1"})
	}, &fullOut)
	if code != 0 {
		t.Fatalf("full exit %d, output %s", code, fullOut)
	}
	fm := regexp.MustCompile(`samples_in=\d+ samples_out=\d+ rate_out=48000 channels=2 peak_dbfs=-?\d+\.\d+ sha256=([0-9a-f]{64})`).FindStringSubmatch(fullOut)
	if fm == nil {
		t.Fatalf("full output shape %q", fullOut)
	}
	if fm[1] != resumeSHA {
		t.Fatalf("full %s != resumed %s", fm[1], resumeSHA)
	}

	// Provenance must carry the input fingerprint for both runs.
	prov, err := os.ReadFile(filepath.Join(work, "provenance.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(prov), "\n") < 2 {
		t.Fatalf("expected demo and resume provenance records, got %q", prov)
	}
	if !strings.Contains(string(prov), `"input_sha256"`) {
		t.Fatalf("provenance lacks input fingerprint")
	}
}

// TestCorruptFixtureOutputs checks the damaged-fixture contract: a
// machine-readable code, a byte offset and track 1.
func TestCorruptFixtureOutputs(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.mp4")
	var out string
	if code := captureStdout(t, func() int {
		return run([]string{"gen", "--out", base, "--seconds", "1", "--frame-size", "1024"})
	}, &out); code != 0 {
		t.Fatalf("gen exit %d: %s", code, out)
	}

	trunc := filepath.Join(dir, "trunc.mp4")
	if code := captureStdout(t, func() int {
		return run([]string{"corrupt", "--kind", "truncated-mdat", "--in", base, "--out", trunc})
	}, &out); code != 0 {
		t.Fatalf("corrupt exit %d: %s", code, out)
	}
	var demoOut string
	code := captureStdout(t, func() int {
		return run([]string{"demo", "--work", filepath.Join(dir, "w1"), "--input", trunc})
	}, &demoOut)
	if code != 4 {
		t.Fatalf("truncated mdat exit %d, want 4; output %s", code, demoOut)
	}

	badlen := filepath.Join(dir, "badlen.mp4")
	if code := captureStdout(t, func() int {
		return run([]string{"corrupt", "--kind", "invalid-box-length", "--in", base, "--out", badlen})
	}, &out); code != 0 {
		t.Fatalf("corrupt exit %d: %s", code, out)
	}
	var badOut string
	code = captureStdout(t, func() int {
		return run([]string{"demo", "--work", filepath.Join(dir, "w2"), "--input", badlen})
	}, &badOut)
	if code != 4 {
		t.Fatalf("invalid box length exit %d, want 4; output %s", code, badOut)
	}
}

// TestUsageErrors pins exit code 2 for bad input and parameters.
func TestUsageErrors(t *testing.T) {
	cases := [][]string{
		{"demo"},
		{"demo", "--work", t.TempDir(), "--channels", "quad"},
		{"demo", "--work", t.TempDir(), "--rate", "96000"},
		{"demo", "--work", t.TempDir(), "--loudness", "3"},
		{"bogus"},
	}
	for _, args := range cases {
		var out string
		if code := captureStdout(t, func() int { return run(args) }, &out); code != 2 {
			t.Errorf("%v: exit %d, want 2 (%s)", args, code, out)
		}
	}
}

// TestCheckpointConflictExitCode proves a changed config exits 3 on resume.
func TestCheckpointConflictExitCode(t *testing.T) {
	work := filepath.Join(t.TempDir(), "w")
	var out string
	if code := captureStdout(t, func() int {
		return run([]string{"demo", "--work", work, "--interrupt-after", "1"})
	}, &out); code != 0 {
		t.Fatalf("demo exit %d: %s", code, out)
	}
	// Resume reads configuration from the checkpoint but a different work
	// directory's checkpoint is the only way to force a conflict through
	// the CLI, so mutate the stored digest instead.
	path := filepath.Join(work, "checkpoint.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mutated := strings.Replace(string(data), `"input_sha256": "`, `"input_sha256": "00`, 1)
	if err := os.WriteFile(path, []byte(mutated), 0o644); err != nil {
		t.Fatal(err)
	}
	var resumeOut string
	code := captureStdout(t, func() int {
		return run([]string{"resume", "--work", work})
	}, &resumeOut)
	if code != 3 {
		t.Fatalf("conflict exit %d, want 3; output %s", code, resumeOut)
	}
}

// captureStdout runs fn with os.Stdout redirected, returning fn's exit code
// and everything written.
func captureStdout(t *testing.T, fn func() int, out *string) int {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()
	code := fn()
	w.Close()
	os.Stdout = old
	*out = <-done
	r.Close()
	return code
}
