package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"64MiB": 64 << 20,
		"1KiB":  1024,
		"2GiB":  2 << 30,
		"1000":  1000,
		"64MB":  64_000_000,
	}
	for in, want := range cases {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Fatalf("%s -> %d %v, want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "-1", "0", "12XiB"} {
		if _, err := ParseSize(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestParseVerifyFlags(t *testing.T) {
	var errBuf bytes.Buffer
	f, err := parseVerifyFlags([]string{"--self-generate", "--segments", "9", "--memory-limit", "32MiB", "--fault", "truncate"}, &errBuf)
	if err != nil {
		t.Fatal(err)
	}
	if !f.selfGenerate || f.segments != 9 || f.memoryLimit != "32MiB" || f.fault != "truncate" {
		t.Fatalf("flags: %+v", f)
	}
	for _, args := range [][]string{
		{"--segments", "0"},
		{"--segments", "601"},
		{"--fault", "bogus"},
		{"--simulate-crash-after", "-1"},
		{}, // neither self-generate nor input
	} {
		if _, err := parseVerifyFlags(args, &errBuf); err == nil {
			t.Fatalf("args %v accepted", args)
		}
	}
}

func TestRunUsageAndUnknownCommand(t *testing.T) {
	var out, errBuf bytes.Buffer
	if rc := run(nil, &out, &errBuf); rc != 1 {
		t.Fatalf("no args rc=%d", rc)
	}
	if rc := run([]string{"nope"}, &out, &errBuf); rc != 1 {
		t.Fatalf("unknown command rc=%d", rc)
	}
	if rc := run([]string{"help"}, &out, &errBuf); rc != 0 {
		t.Fatalf("help rc=%d", rc)
	}
	if !strings.Contains(out.String(), "mediacore verify") {
		t.Fatalf("usage missing: %q", out.String())
	}
}

func TestVerifyCleanRun(t *testing.T) {
	var out, errBuf bytes.Buffer
	rc := run([]string{"verify", "--self-generate", "--segments", "3", "--memory-limit", "8MiB"}, &out, &errBuf)
	if rc != 0 {
		t.Fatalf("clean run rc=%d stderr=%s", rc, errBuf.String())
	}
	s := out.String()
	for _, want := range []string{
		"roundtrip_rate=100.00%",
		"keyframe_hit_rate=100.00%",
		"segment_sha256_match=3/3",
		"queue_peak_bytes=",
		"quarantined_records=0",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
	if !strings.Contains(s, "queue_limit_bytes=8388608") {
		t.Fatalf("memory limit not echoed:\n%s", s)
	}
}

func TestVerifyTruncatedReturnsExit2(t *testing.T) {
	var out, errBuf bytes.Buffer
	rc := run([]string{"verify", "--self-generate", "--fault", "truncate", "--segments", "4"}, &out, &errBuf)
	if rc != 2 {
		t.Fatalf("truncate rc=%d stderr=%s", rc, errBuf.String())
	}
	if !strings.Contains(out.String(), "quarantined_records=1") {
		t.Fatalf("quarantine not reported:\n%s", out.String())
	}
}

func TestVerifyRecovery(t *testing.T) {
	var out, errBuf bytes.Buffer
	rc := run([]string{"verify", "--self-generate", "--fault", "truncate", "--segments", "8",
		"--simulate-crash-after", "5", "--resume"}, &out, &errBuf)
	if rc != 2 {
		t.Fatalf("recovery rc=%d stderr=%s", rc, errBuf.String())
	}
	s := out.String()
	for _, want := range []string{
		"index_invalidated=1",
		"recovered_segments=5",
		"resumed=true",
		"segment_sha256_match=8/8",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
}

func TestVerifyStaleIndex(t *testing.T) {
	var out, errBuf bytes.Buffer
	rc := run([]string{"verify", "--self-generate", "--fault", "stale-index", "--segments", "4"}, &out, &errBuf)
	if rc != 0 {
		t.Fatalf("stale-index rc=%d stderr=%s", rc, errBuf.String())
	}
	s := out.String()
	if !strings.Contains(s, "index_invalidated=1") || !strings.Contains(s, "index_rebuilt=1") {
		t.Fatalf("stale index not invalidated:\n%s", s)
	}
}

func TestVerifyBadInputReturns1(t *testing.T) {
	var out, errBuf bytes.Buffer
	rc := run([]string{"verify", "--input", "/nonexistent/file.mp4"}, &out, &errBuf)
	if rc != 1 {
		t.Fatalf("missing input rc=%d", rc)
	}
}

func TestVerifySelfGeneratedFixtureOnlyInWorkDir(t *testing.T) {
	dir := t.TempDir()
	var out, errBuf bytes.Buffer
	rc := run([]string{"verify", "--self-generate", "--segments", "2", "--work-dir", dir, "--keep"}, &out, &errBuf)
	if rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, errBuf.String())
	}
	if _, err := os.Stat(dir + "/self.mp4"); err != nil {
		t.Fatalf("fixture not kept: %v", err)
	}
}
