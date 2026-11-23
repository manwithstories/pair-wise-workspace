package main

import (
	"bytes"
	"strings"
	"testing"
)

func runCLI(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String()
}

func TestDemoHappyPathContract(t *testing.T) {
	code, out := runCLI(t, "demo",
		"--seed", "41", "--loss-burst", "7", "--duplicate", "3", "--reorder", "5",
		"--bandwidth-kbps", "1800", "--target-kbps", "2000",
		"--checkpoint-samples", "120", "--resume")
	if code != 0 {
		t.Fatalf("exit = %d, out:\n%s", code, out)
	}
	required := []string{
		"track_count=", "sample_count=", "target_kbps=2000", "actual_kbps=",
		"bitrate_error_pct=", "sample_sha256_match=true",
		"quality_mean=", "quality_p95=", "manifest_alignment_ms=",
		"resumed_blocks=", "final_hash=",
	}
	for _, key := range required {
		if !strings.Contains(out, key) {
			t.Errorf("missing %q in output:\n%s", key, out)
		}
	}
	if !strings.Contains(out, "interrupted_after_blocks=") {
		t.Errorf("resume run should report the interruption:\n%s", out)
	}
}

func TestCorruptTruncatedExitCodeAndOffset(t *testing.T) {
	code, out := runCLI(t, "demo", "--corrupt", "truncated-mp4")
	if code != 2 {
		t.Fatalf("exit = %d, want 2\n%s", code, out)
	}
	if !strings.Contains(out, "error_code=TRUNCATED_BOX") {
		t.Fatalf("missing TRUNCATED_BOX:\n%s", out)
	}
	if !strings.Contains(out, "offset=") {
		t.Fatalf("missing offset:\n%s", out)
	}
	if !strings.Contains(out, "checkpoint_recovered=") {
		t.Fatalf("missing checkpoint_recovered:\n%s", out)
	}
}

func TestParamErrorsExitOne(t *testing.T) {
	cases := [][]string{
		{"demo", "--checkpoint-samples", "0"},
		{"demo", "--rotation", "45"},
		{"demo", "--target-kbps", "-5"},
		{"demo", "--corrupt", "bogus"},
		{"nonsense"},
		{"demo", "--not-a-flag"},
	}
	for _, args := range cases {
		code, out := runCLI(t, args...)
		if code != 1 {
			t.Fatalf("%v exit = %d, want 1\n%s", args, code, out)
		}
		if len(args) > 0 && args[0] == "demo" && !strings.Contains(out, "error_code=PARAM_INVALID") {
			t.Fatalf("%v missing PARAM_INVALID:\n%s", args, out)
		}
	}
}

func TestNoArgsShowsUsage(t *testing.T) {
	code, out := runCLI(t)
	if code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(strings.ToLower(out), "flags:") {
		t.Fatalf("expected usage text, got:\n%s", out)
	}
}

func TestOutputKeysAreSingleLine(t *testing.T) {
	_, out := runCLI(t, "demo", "--corrupt", "invalid-klv")
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		if !strings.Contains(line, "=") {
			t.Fatalf("non key=value line: %q", line)
		}
		if strings.Count(line, "\n") != 0 {
			t.Fatalf("multi-line value: %q", line)
		}
	}
}
