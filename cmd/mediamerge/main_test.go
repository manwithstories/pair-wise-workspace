package main

import (
	"bytes"
	"strings"
	"testing"
)

type capture struct{ bytes.Buffer }

func (c *capture) String() string { return c.Buffer.String() }

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errBuf capture
	code := run(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func TestUsageAndUnknownCommand(t *testing.T) {
	if code, _, _ := runCLI(t); code != exitUsage {
		t.Fatalf("no args: %d", code)
	}
	if code, _, errOut := runCLI(t, "nonsense"); code != exitUsage || !strings.Contains(errOut, "unknown command") {
		t.Fatalf("unknown: %d %q", code, errOut)
	}
	if code, out, _ := runCLI(t, "help"); code != exitOK || !strings.Contains(out, "usage:") {
		t.Fatalf("help: %d", code)
	}
	if code, _, _ := runCLI(t, "demo", "--chunk-bytes", "0", "--work-dir", t.TempDir()); code != exitFail {
		t.Fatalf("bad option should fail at run time: %d", code)
	}
	if code, _, _ := runCLI(t, "demo", "--nope"); code != exitUsage {
		t.Fatalf("unknown flag: %d", code)
	}
}

func TestDemoNormalPathPrintsMetrics(t *testing.T) {
	dir := t.TempDir()
	code, out, errOut := runCLI(t,
		"demo", "--seed", "42", "--chunk-bytes", "16384", "--consumer-delay-ms", "0",
		"--work-dir", dir)
	if code != exitOK {
		t.Fatalf("demo failed: %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{
		"PARSE_ROUNDTRIP_RATE 1.000", "KEYFRAME_HIT_RATE 1.000", "SEGMENT_HASH_MATCH",
		"WAL_RECOVERED 0", "COMMITTED_STAGES 4", "ROLLED_BACK_BYTES 0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
	// 8/8 or better, exact fraction.
	matched, total := parseFraction(t, out, "SEGMENT_HASH_MATCH ")
	if matched != total || matched < 8 {
		t.Fatalf("segment hash %d/%d", matched, total)
	}
}

func TestDemoRecoveryPathPrintsRecovery(t *testing.T) {
	dir := t.TempDir()
	code, out, errOut := runCLI(t,
		"demo", "--seed", "42", "--chunk-bytes", "16384", "--consumer-delay-ms", "0",
		"--fail-after-stage", "2", "--resume", "--work-dir", dir)
	if code != exitOK {
		t.Fatalf("recovery failed: %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"INTERRUPTED stage=mux", "WAL_RECOVERED 1", "COMMITTED_STAGES 4"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
	rolled := parseField(t, out, "ROLLED_BACK_BYTES ")
	if rolled <= 0 {
		t.Fatalf("rolled back %d", rolled)
	}
	if !strings.Contains(out, "SEGMENT_HASH_MATCH") {
		t.Fatalf("recovery did not report segment hashes:\n%s", out)
	}
}

func TestCorruptFixturesExitNonZeroWithLocation(t *testing.T) {
	cases := map[string]string{
		"truncated-box":       "TRUNCATED_BOX",
		"truncated-partition": "TRUNCATED_MXF_PARTITION",
		"non-monotonic":       "NON_MONOTONIC_TIME",
		"bad-crop":            "CROP_OUT_OF_BOUNDS",
	}
	for kind, code := range cases {
		dir := t.TempDir()
		rc, out, _ := runCLI(t, "demo", "--seed", "42", "--corrupt", kind, "--work-dir", dir)
		if rc != exitFail {
			t.Fatalf("%s: exit %d\n%s", kind, rc, out)
		}
		if !strings.Contains(out, code) {
			t.Fatalf("%s: missing %s:\n%s", kind, code, out)
		}
		if !strings.Contains(out, "CORRUPT_LOCATED 1") {
			t.Fatalf("%s: no location marker:\n%s", kind, out)
		}
	}
	if rc, _, _ := runCLI(t, "demo", "--corrupt", "wat", "--work-dir", t.TempDir()); rc != exitUsage {
		t.Fatalf("unknown corruption: %d", rc)
	}
}

func TestVerifyAfterDemo(t *testing.T) {
	dir := t.TempDir()
	if code, out, errOut := runCLI(t, "demo", "--seed", "7", "--chunk-bytes", "16384",
		"--consumer-delay-ms", "0", "--work-dir", dir); code != exitOK {
		t.Fatalf("demo: %d\n%s\n%s", code, out, errOut)
	}
	code, out, errOut := runCLI(t, "verify", "--work-dir", dir)
	if code != exitOK {
		t.Fatalf("verify: %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "COMMITTED_STAGES 4") {
		t.Fatalf("verify output:\n%s", out)
	}
	if code, _, _ := runCLI(t, "verify", "--work-dir", t.TempDir()); code != exitFail {
		t.Fatalf("verify of empty dir should fail: %d", code)
	}
}
