package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediad/internal/fixture"
)

func runCLI(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return code, out.String() + errOut.String()
}

func TestDemoExitZeroAndMetrics(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "batch")
	code, out := runCLI(t, "demo", "--seed", "42", "--chunk-size", "8", "--workers", "4",
		"--resume", "--segments", "12", "--dir", dir)
	if code != 0 {
		t.Fatalf("exit=%d output=%s", code, out)
	}
	for _, want := range []string{
		"manifest_roundtrip=100.0%", "keyframe_hit=100.0%", "segments_ok=",
		"segments_bad=", "recovered_chunks=2", "sha256_match=", "timeline_gaps=",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "summary manifest_roundtrip=100.0%") {
		t.Fatalf("summary line missing:\n%s", out)
	}
}

func TestVerifyAcceptsDemoOutput(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "batch")
	if code, out := runCLI(t, "demo", "--segments", "9", "--dir", dir); code != 0 {
		t.Fatalf("demo failed: %d %s", code, out)
	}
	code, out := runCLI(t, "verify", "--input", dir)
	if code != 0 {
		t.Fatalf("verify exit=%d output=%s", code, out)
	}
	if !strings.Contains(out, "problems=0") {
		t.Fatalf("verify output: %s", out)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "batch")
	if code, out := runCLI(t, "demo", "--segments", "6", "--dir", dir); code != 0 {
		t.Fatalf("demo failed: %d %s", code, out)
	}
	segment := filepath.Join(dir, "normal", "seg002.m4s")
	if err := os.WriteFile(segment, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := runCLI(t, "verify", "--input", dir)
	if code != exitFailure {
		t.Fatalf("verify exit=%d output=%s", code, out)
	}
	if !strings.Contains(out, "expected=") {
		t.Fatalf("verify did not report mismatch: %s", out)
	}
}

func TestUsageErrors(t *testing.T) {
	cases := [][]string{
		{},
		{"unknown"},
		{"demo", "--chunk-size", "0"},
		{"demo", "--workers", "0"},
		{"demo", "--segments", "0"},
		{"verify"},
		{"verify", "--input", filepath.Join(t.TempDir(), "missing")},
	}
	for _, args := range cases {
		code, out := runCLI(t, args...)
		if code != exitUsage {
			t.Fatalf("args %v exit=%d output=%s", args, code, out)
		}
	}
}

func TestReportJSONShape(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "batch")
	if code, out := runCLI(t, "demo", "--segments", "6", "--dir", dir); code != 0 {
		t.Fatalf("demo failed: %d %s", code, out)
	}
	raw, err := os.ReadFile(filepath.Join(dir, fixture.ReportName))
	if err != nil {
		t.Fatal(err)
	}
	var rep demoReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Cases) != len(fixture.AllCases) {
		t.Fatalf("cases = %d", len(rep.Cases))
	}
	for _, c := range rep.Cases {
		if c.SegmentHashes == nil {
			t.Fatalf("case %s has no evidence", c.Case)
		}
		for _, ev := range c.SegmentHashes {
			if len(ev.SHA256) != 64 || ev.Path == "" {
				t.Fatalf("bad evidence %+v", ev)
			}
			if filepath.IsAbs(ev.Path) || strings.HasPrefix(ev.Path, "..") {
				t.Fatalf("evidence path escapes batch: %s", ev.Path)
			}
		}
	}
}

func TestDemoIsReproducibleAcrossDirs(t *testing.T) {
	dirA := filepath.Join(t.TempDir(), "a")
	dirB := filepath.Join(t.TempDir(), "b")
	if code, out := runCLI(t, "demo", "--segments", "6", "--dir", dirA); code != 0 {
		t.Fatalf("demo A failed: %d %s", code, out)
	}
	if code, out := runCLI(t, "demo", "--segments", "6", "--dir", dirB); code != 0 {
		t.Fatalf("demo B failed: %d %s", code, out)
	}
	reportA, err := os.ReadFile(filepath.Join(dirA, fixture.ReportName))
	if err != nil {
		t.Fatal(err)
	}
	reportB, err := os.ReadFile(filepath.Join(dirB, fixture.ReportName))
	if err != nil {
		t.Fatal(err)
	}
	if string(reportA) != string(reportB) {
		t.Fatal("reports differ between identical runs")
	}
}
