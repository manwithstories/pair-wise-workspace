package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediarun/internal/cli"
	"mediarun/internal/mediaerr"
)

func find(lines []string, key string) string {
	for _, l := range lines {
		if strings.HasPrefix(l, key+"=") {
			return strings.TrimPrefix(l, key+"=")
		}
	}
	return ""
}

func TestDemoSuccessOutput(t *testing.T) {
	work := t.TempDir()
	a, err := cli.Parse([]string{"demo", "--work", work, "--queue-depth", "4",
		"--target-bitrate", "12000000"})
	if err != nil {
		t.Fatal(err)
	}
	lines, stats, err := cli.RunDemo(a)
	if err != nil {
		t.Fatalf("RunDemo: %v", err)
	}
	want := map[string]string{
		"samples_in": "240", "samples_out": "240", "quality_checked": "240",
		"quality_errors": "0", "target_bitrate_bps": "12000000",
		"actual_bitrate_bps": "12000000", "bitrate_deviation_pct": "0.0",
		"manifest_alignment_ms": "0", "output_committed": "true",
	}
	for k, v := range want {
		if got := find(lines, k); got != v {
			t.Errorf("field %s = %q, want %q", k, got, v)
		}
	}
	if find(lines, "output_sha256") == "" {
		t.Error("output_sha256 missing")
	}
	if stats == nil || !stats.OutputCommitted {
		t.Fatal("stats must show committed output")
	}
	// Trace manifest must be written next to the output.
	if _, err := os.ReadFile(filepath.Join(work, "trace.json")); err != nil {
		t.Fatalf("trace manifest missing: %v", err)
	}
}

func TestDemoCorruptReportsStructuredError(t *testing.T) {
	work := t.TempDir()
	a, err := cli.Parse([]string{"demo", "--work", work, "--inject", "truncated-box"})
	if err != nil {
		t.Fatal(err)
	}
	lines, _, err := cli.RunDemo(a)
	if !mediaerr.Is(err, mediaerr.CodeTruncatedBox) {
		t.Fatalf("want TRUNCATED_BOX, got %v", err)
	}
	if find(lines, "error_code") != "TRUNCATED_BOX" {
		t.Fatalf("error_code line wrong: %v", lines)
	}
	if find(lines, "input_offset") == "" || find(lines, "input_offset") == "-1" {
		t.Fatalf("input_offset must be a concrete offset: %v", lines)
	}
	if find(lines, "output_committed") != "false" {
		t.Fatal("corrupt run must not commit output")
	}
}

func TestDemoInterruptAndResume(t *testing.T) {
	work := t.TempDir()
	first, err := cli.Parse([]string{"demo", "--work", work, "--interrupt-after", "97",
		"--target-bitrate", "12000000"})
	if err != nil {
		t.Fatal(err)
	}
	_, s1, err := cli.RunDemo(first)
	if err != nil {
		t.Fatalf("interrupt leg should be clean, got %v", err)
	}
	if s1.SamplesOut != 97 {
		t.Fatalf("first leg out = %d, want 97", s1.SamplesOut)
	}

	// Reference uninterrupted run for hash comparison.
	refWork := t.TempDir()
	refArgs, _ := cli.Parse([]string{"demo", "--work", refWork, "--target-bitrate", "12000000"})
	_, ref, err := cli.RunDemo(refArgs)
	if err != nil {
		t.Fatal(err)
	}

	resumeArgs, err := cli.Parse([]string{"demo", "--work", work, "--resume",
		"--target-bitrate", "12000000"})
	if err != nil {
		t.Fatal(err)
	}
	lines, s2, err := cli.RunDemo(resumeArgs)
	if err != nil {
		t.Fatalf("resume leg: %v", err)
	}
	if find(lines, "resume_from_sample") != "97" {
		t.Fatalf("resume_from_sample line missing: %v", lines)
	}
	if s2.SamplesOut != 240 || s2.DuplicateSamples != 0 {
		t.Fatalf("resume stats = %+v", s2)
	}
	if s2.OutputSHA256 != ref.OutputSHA256 {
		t.Fatalf("resume hash %s != reference %s", s2.OutputSHA256, ref.OutputSHA256)
	}
}

func TestDemoMXFFormat(t *testing.T) {
	work := t.TempDir()
	a, err := cli.Parse([]string{"demo", "--work", work, "--format", "mxf",
		"--target-bitrate", "8000000"})
	if err != nil {
		t.Fatal(err)
	}
	lines, _, err := cli.RunDemo(a)
	if err != nil {
		t.Fatalf("MXF demo: %v", err)
	}
	if find(lines, "format") != "mxf" || find(lines, "samples_out") != "240" {
		t.Fatalf("mxf output wrong: %v", lines)
	}
}
