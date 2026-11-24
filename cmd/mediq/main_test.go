package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCLIDemoRequiresOut(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"demo", "--case", "clean"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code=%d, want 1", code)
	}
}

func TestCLICrashCode75AndResume(t *testing.T) {
	dir := t.TempDir()
	var firstOut, firstErr bytes.Buffer
	code := run([]string{
		"demo", "--case", "recovery", "--out", dir, "--crash-after", "checkpoint",
	}, &firstOut, &firstErr)
	if code != 75 {
		t.Fatalf("first code=%d stderr=%s", code, firstErr.String())
	}
	if !strings.Contains(firstOut.String(), "injected_interrupt=checkpoint") {
		t.Fatalf("missing injection marker: %s", firstOut.String())
	}

	var out, stderr bytes.Buffer
	code = run([]string{"demo", "--case", "recovery", "--out", dir, "--resume"}, &out, &stderr)
	if code != 0 {
		t.Fatalf("resume code=%d stderr=%s", code, stderr.String())
	}
	text := out.String()
	for _, key := range []string{
		"target_bps=1536000", "actual_bps=", "deviation_pct=", "missing_samples=",
		"duplicate_packets=", "late_replays=", "integrated_lufs=", "true_peak_dbtp=",
		"manifest_error_ms=", "product_sha256=",
	} {
		if !strings.Contains(text, key) {
			t.Fatalf("output missing %q\n%s", key, text)
		}
	}
}

func TestCLIStrictReturnsTwo(t *testing.T) {
	var out, stderr bytes.Buffer
	code := run([]string{"demo", "--case", "damaged", "--out", t.TempDir(), "--strict"}, &out, &stderr)
	if code != 2 {
		t.Fatalf("strict code=%d stderr=%s", code, stderr.String())
	}
}
