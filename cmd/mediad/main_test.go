package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runCLI invokes run with captured streams so exit codes and field output are
// asserted exactly as the terminal contract requires.
func runCLI(args ...string) (code int, stdout, stderr string) {
	var out, errBuf bytes.Buffer
	code = run(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func TestSuccessCommandFieldsAndExitZero(t *testing.T) {
	out := t.TempDir()
	code, stdout, stderr := runCLI("demo", "--seed", "42", "--bandwidth-kbps", "1800",
		"--loss-burst", "4", "--retry", "2", "--out", out)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr)
	}
	required := []string{"target_kbps=", "actual_kbps=", "bitrate_error_pct=",
		"frame_count=", "psnr_mean_db=", "ssim_mean=", "manifest_drift_ms=",
		"duplicate_packets=", "reordered_packets=", "lost_packets=", "blocked_node="}
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			fields[k] = v
		}
	}
	for _, key := range required {
		if _, ok := fields[strings.TrimSuffix(key, "=")]; !ok {
			t.Fatalf("missing field %q in:\n%s", key, stdout)
		}
	}
	if fields["blocked_node"] != "none" {
		t.Fatalf("blocked_node = %s", fields["blocked_node"])
	}
	// The manifest written by the CLI must exist and be valid JSON.
	raw, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("bad manifest JSON: %v", err)
	}
}

func TestCorruptMetadataExitsTwoWithProvenance(t *testing.T) {
	out := t.TempDir()
	code, stdout, stderr := runCLI("demo", "--seed", "42", "--corrupt-metadata", "--out", out)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stderr, "category=metadata") {
		t.Fatalf("stderr lacks category: %s", stderr)
	}
	if !strings.Contains(stderr, "track=v0") {
		t.Fatalf("stderr lacks track: %s", stderr)
	}
	if !strings.Contains(stderr, "blocked_node=") {
		t.Fatalf("stderr lacks blocking dependency: %s", stderr)
	}
	if !strings.Contains(stdout, "blocked_node=") {
		t.Fatalf("stdout lacks blocked_node: %s", stdout)
	}
}

func TestUsageErrorsExitOne(t *testing.T) {
	cases := [][]string{
		{},
		{"demo"},
		{"demo", "--out", t.TempDir(), "--bandwidth-kbps", "0"},
		{"demo", "--out", t.TempDir(), "--retry", "-1"},
		{"demo", "--out", t.TempDir(), "--render-scale", "0"},
		{"demo", "--out", t.TempDir(), "--scenario", "nope"},
		{"demo", "--out", t.TempDir(), "--unknown-flag"},
		{"frobnicate"},
	}
	for _, args := range cases {
		code, _, stderr := runCLI(args...)
		if code != 1 {
			t.Fatalf("%v exit = %d, want 1 (stderr=%s)", args, code, stderr)
		}
	}
}

func TestBurstScenarioReportsBlockedNode(t *testing.T) {
	out := t.TempDir()
	code, stdout, _ := runCLI("demo", "--out", out, "--scenario", "burst-loss", "--loss-burst", "4")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "lost_packets=4") {
		t.Fatalf("loss not reported:\n%s", stdout)
	}
	if !strings.Contains(stdout, "blocked_node=transport:gap") {
		t.Fatalf("burst did not block frames:\n%s", stdout)
	}
}

func TestWrapAndDuplicateScenarios(t *testing.T) {
	out := t.TempDir()
	code, stdout, stderr := runCLI("demo", "--out", out, "--scenario", "wrap")
	if code != 0 {
		t.Fatalf("wrap exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "wrapped=true") {
		t.Fatalf("wrap not reported:\n%s", stdout)
	}
	out2 := t.TempDir()
	code, stdout, _ = runCLI("demo", "--out", out2, "--scenario", "duplicate")
	if code != 0 {
		t.Fatalf("duplicate exit=%d", code)
	}
	if strings.Contains(stdout, "duplicate_packets=0\n") {
		t.Fatalf("duplicates not reported:\n%s", stdout)
	}
}

func TestResumeFromManifest(t *testing.T) {
	first := t.TempDir()
	if code, _, stderr := runCLI("demo", "--out", first); code != 0 {
		t.Fatalf("first run exit=%d %s", code, stderr)
	}
	second := t.TempDir()
	code, stdout, stderr := runCLI("demo", "--out", second,
		"--resume-from", filepath.Join(first, "manifest.json"))
	if code != 0 {
		t.Fatalf("resume exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "blocked_node=none") {
		t.Fatalf("resume blocked:\n%s", stdout)
	}
	// Re-running the acceptance command over the same output directory must
	// work (the directory is cleanable and rebuildable).
	if code, _, stderr := runCLI("demo", "--out", first); code != 0 {
		t.Fatalf("rerun exit=%d %s", code, stderr)
	}
}

func TestHelpAndVersion(t *testing.T) {
	if code, stdout, _ := runCLI("help"); code != 0 || !strings.Contains(stdout, "Usage:") {
		t.Fatal("help failed")
	}
	if code, stdout, _ := runCLI("version"); code != 0 || !strings.Contains(stdout, "mediad") {
		t.Fatal("version failed")
	}
}
