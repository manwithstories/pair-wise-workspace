package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// chdirTemp runs the test inside a fresh directory so the CLI's ./tmp work
// directory cannot collide with a developer's files.
func chdirTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	return dir
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := run(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

var (
	reSummary = regexp.MustCompile(`tracks=(\d+) samples=(\d+) max_queue_bytes=(\d+) remux_sha256_match=(true|false)`)
	reFault   = regexp.MustCompile(`error_class=(\S+) input_offset=(-?\d+) tolerance_bytes=(\d+)`)
	reResume  = regexp.MustCompile(`resume_ms=(\d+) replay_samples=(\d+) output_sha256=([0-9a-f]+)`)
)

func TestDemoSuccessOutput(t *testing.T) {
	chdirTemp(t)
	code, out, errOut := runCLI(t, "demo", "--generate", "--format", "mixed",
		"--queue-bytes", "1048576", "--consumer-delay", "20ms",
		"--checkpoint", "./tmp/checkpoint.json")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s stdout=%s", code, errOut, out)
	}
	m := reSummary.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("summary line missing in %q", out)
	}
	if m[1] != "3" {
		t.Fatalf("tracks=%s", m[1])
	}
	if m[4] != "true" {
		t.Fatalf("remux_sha256_match=%s", m[4])
	}
	if m[3] == "0" {
		t.Fatalf("max_queue_bytes was never observed")
	}
	// The byte budget must be respected.
	peak, err := strconv.ParseInt(m[3], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if peak > 1048576 {
		t.Fatalf("max_queue_bytes=%d exceeds the configured budget", peak)
	}
	if _, err := os.Stat("./tmp/checkpoint.json"); err != nil {
		t.Fatalf("checkpoint not written: %v", err)
	}
}

func TestDemoTruncateReportsExactOffset(t *testing.T) {
	chdirTemp(t)
	code, out, _ := runCLI(t, "demo", "--generate", "--corrupt", "truncate", "--offset", "4096")
	if code != 2 {
		t.Fatalf("exit=%d want 2 (stdout=%s)", code, out)
	}
	m := reFault.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("fault line missing in %q", out)
	}
	if m[1] != "truncated" || m[2] != "4096" || m[3] != "0" {
		t.Fatalf("fault=%v", m)
	}
}

func TestDemoInvalidSampleLengthReportsTableOffset(t *testing.T) {
	chdirTemp(t)
	code, out, _ := runCLI(t, "demo", "--generate", "--corrupt", "sample_length", "--offset", "3")
	if code != 2 {
		t.Fatalf("exit=%d want 2 (stdout=%s)", code, out)
	}
	m := reFault.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("fault line missing in %q", out)
	}
	if m[1] != "invalid_length" {
		t.Fatalf("class=%s", m[1])
	}
	if m[2] == "-1" || m[2] == "0" {
		t.Fatalf("expected a real table offset, got %s", m[2])
	}
}

func TestDemoInterruptResumeIsStableAcrossRepeats(t *testing.T) {
	chdirTemp(t)
	// First run creates and completes a delivery with a checkpoint.
	if code, out, errOut := runCLI(t, "demo", "--generate", "--format", "mixed",
		"--checkpoint", "./tmp/checkpoint.json"); code != 0 {
		t.Fatalf("first run exit=%d stderr=%s", code, errOut)
	} else if !strings.Contains(out, "remux_sha256_match=true") {
		t.Fatalf("first run summary: %s", out)
	}

	var hashes []string
	for i := 0; i < 2; i++ {
		code, out, errOut := runCLI(t, "demo", "--generate",
			"--interrupt-after", "120", "--resume", "./tmp/checkpoint.json")
		if code != 0 {
			t.Fatalf("resume exit=%d stderr=%s", code, errOut)
		}
		m := reResume.FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("resume line missing in %q", out)
		}
		if m[2] != "0" {
			t.Fatalf("replay_samples=%s", m[2])
		}
		hashes = append(hashes, m[3])
	}
	if hashes[0] != hashes[1] {
		t.Fatalf("output checksums differ across repeated resume runs: %v", hashes)
	}
}

func TestDemoUsageErrorsExitOne(t *testing.T) {
	chdirTemp(t)
	cases := [][]string{
		{"demo", "--generate", "--format", "nope"},
		{"demo", "--generate", "--corrupt", "truncate"},
		{"demo", "--generate", "--queue-bytes", "0"},
		{"demo", "--generate", "--corrupt", "bogus"},
		{"demo"}, // missing --generate
		{"demo", "--generate", "--corrupt", "wrap"},           // wrap needs MXF
		{"demo", "--generate", "--corrupt", "ambiguous_wrap"}, // wrap needs MXF
	}
	for _, args := range cases {
		if code, out, _ := runCLI(t, args...); code != 1 {
			t.Fatalf("args=%v exit=%d want 1 (stdout=%s)", args, code, out)
		}
	}
}

func TestDemoCancelPropagates(t *testing.T) {
	chdirTemp(t)
	code, out, _ := runCLI(t, "demo", "--generate", "--corrupt", "cancel")
	if code != 2 {
		t.Fatalf("exit=%d want 2 (stdout=%s)", code, out)
	}
	m := reFault.FindStringSubmatch(out)
	if m == nil || m[1] != "cancelled" {
		t.Fatalf("stdout=%s", out)
	}
	if !strings.Contains(out, "consumer_cancelled=true") {
		t.Fatalf("cancellation not demonstrated: %s", out)
	}
}

// TestLargeLogicalStreamUsesBoundedMemory asserts the 1 GiB logical stream
// requirement: the fixture is sparse, so its logical size is large while no
// file is read into memory.
func TestLargeLogicalStreamUsesBoundedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("large logical stream test skipped in short mode")
	}
	dir := chdirTemp(t)
	code, out, errOut := runCLI(t, "demo", "--generate", "--big", "--tracks", "1",
		"--queue-bytes", "1048576", "--queue-samples", "8")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	m := reSummary.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("summary missing: %s", out)
	}
	if m[4] != "true" {
		t.Fatalf("checksum mismatch on large stream")
	}
	info, err := os.Stat(filepath.Join(dir, "tmp", "fixture.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 800<<20 {
		t.Fatalf("logical fixture is only %d bytes", info.Size())
	}
}
