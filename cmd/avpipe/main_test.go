package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func execCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := run(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func TestNoArgsIsUsageError(t *testing.T) {
	code, _, stderr := execCLI(t)
	if code != 2 || !strings.Contains(stderr, "usage:") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestUnknownSubcommandIsUsageError(t *testing.T) {
	code, _, stderr := execCLI(t, "frobnicate")
	if code != 2 || !strings.Contains(stderr, "unknown subcommand") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestRunPassesWithDefaults(t *testing.T) {
	code, stdout, stderr := execCLI(t, "run", "--seed", "42")
	if code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	for _, want := range []string{"sync_p95_ms", "latency_p95_ms", "packet_recovery_rate",
		"queue_peak_bytes", "rms_before", "rms_after", "root_error null", "result=PASS"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestSelftestScenariosAreShown(t *testing.T) {
	code, stdout, _ := execCLI(t, "selftest", "--seed", "42", "--jitter-ms", "35",
		"--consumer-delay-ms", "18", "--queue-bytes", "262144")
	if code != 0 {
		t.Fatalf("code=%d stdout=%s", code, stdout)
	}
	for _, name := range []string{"corrupt_wav", "recovery_window", "queue_close", "graph_cycle"} {
		if !strings.Contains(stdout, "scenario "+name) {
			t.Fatalf("missing scenario %s:\n%s", name, stdout)
		}
	}
}

func TestInvalidLayoutExitsTwo(t *testing.T) {
	code, _, stderr := execCLI(t, "run", "--layout", "spatial-11")
	if code != 2 || !strings.Contains(stderr, "unsupported_layout") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestInvalidPolicyExitsTwo(t *testing.T) {
	code, _, stderr := execCLI(t, "run", "--policy", "explode")
	if code != 2 || !strings.Contains(stderr, "policy") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestUnsupportedInputExitsTwo(t *testing.T) {
	code, _, stderr := execCLI(t, "run", "--input", "/tmp/x.wav")
	if code != 2 || !strings.Contains(stderr, "not supported") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestOutOfRangeFlagsExitTwo(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--loss", "1.5"},
		{"run", "--loss", "-0.1"},
		{"run", "--jitter-ms", "-5"},
		{"run", "--queue-bytes", "0"},
		{"run", "--sample-rate", "100"},
		{"run", "--block-size", "0"},
		{"run", "--recovery-window", "0"},
		{"run", "--frames", "1", "--block-size", "4096"},
		{"run", "--target-lufs", "12"},
	} {
		code, _, stderr := execCLI(t, args...)
		if code != 2 {
			t.Fatalf("%v: code=%d stderr=%q", args, code, stderr)
		}
	}
}

func TestMetricFailureExitsThree(t *testing.T) {
	// A consumer far slower than the block cadence overruns the slow-consumer
	// latency budget once the queue depth is tiny.
	code, stdout, _ := execCLI(t, "run", "--seed", "7", "--consumer-delay-ms", "400",
		"--queue-bytes", "8192", "--queue-ms", "60")
	if code != 3 {
		t.Fatalf("code=%d stdout=%s", code, stdout)
	}
	if !strings.Contains(stdout, "FAIL") || !strings.Contains(stdout, "exit=3") {
		t.Fatalf("expected failure verdict:\n%s", stdout)
	}
}

func TestReportIsWrittenAndValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "report.json")
	code, _, stderr := execCLI(t, "run", "--seed", "42", "--report", path)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, key := range []string{"sync_error_ms", "packet_recovery_rate", "latency_p95_ms",
		"queue_peak_bytes", "rms_before", "rms_after", "root_error", "exit_code", "profile"} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("report missing %q", key)
		}
	}
	if doc["root_error"] != nil {
		t.Fatalf("clean run root_error=%v", doc["root_error"])
	}
}

func TestLayoutFlagDrivesPipeline(t *testing.T) {
	code, stdout, stderr := execCLI(t, "run", "--seed", "1",
		"--source-layout", "stereo", "--layout", "5.1")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "layout=5.1 rate=") {
		t.Fatalf("target layout not honored:\n%s", stdout)
	}
	if !strings.Contains(stdout, "silence_fills                     4") {
		t.Fatalf("stereo->5.1 should report 4 silence fills:\n%s", stdout)
	}
}

func TestSampleRateFlagDrivesPipeline(t *testing.T) {
	code, stdout, _ := execCLI(t, "run", "--seed", "1",
		"--source-sample-rate", "48000", "--sample-rate", "44100")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(stdout, "rate=44100") {
		t.Fatalf("target sample rate not honored:\n%s", stdout)
	}
}

func TestHelpExitsZero(t *testing.T) {
	code, stdout, _ := execCLI(t, "--help")
	if code != 0 || !strings.Contains(stdout, "avpipe run") {
		t.Fatalf("code=%d stdout=%q", code, stdout)
	}
}

func TestDeterministicCLIOutput(t *testing.T) {
	_, a, _ := execCLI(t, "selftest", "--seed", "42", "--loss", "0.08", "--jitter-ms", "35",
		"--consumer-delay-ms", "18", "--queue-bytes", "262144")
	_, b, _ := execCLI(t, "selftest", "--seed", "42", "--loss", "0.08", "--jitter-ms", "35",
		"--consumer-delay-ms", "18", "--queue-bytes", "262144")
	if a != b {
		t.Fatal("identical invocations produced different output")
	}
}
