package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediacheck/internal/report"
)

// runCLI executes the command with captured streams.
func runCLI(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = run(args, &out, &errb)
	return code, out.String(), errb.String()
}

// genAll writes the standard fixture set into a temp dir.
func genAll(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	code, out, errs := runCLI("generate", "--dir", dir)
	if code != report.ExitOK {
		t.Fatalf("generate exit=%d stderr=%s", code, errs)
	}
	for _, name := range []string{"valid.mp4", "truncated.mp4", "corrupt.mp4", "missing-index.mp4", "boundary-mismatch.mp4"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("generate missing %s: %v\n%s", name, err, out)
		}
	}
	return dir
}

func TestGenerateCommand(t *testing.T) {
	dir := genAll(t)
	if code, _, _ := runCLI("generate"); code != report.ExitFailure {
		t.Fatalf("generate without --dir exit=%d", code)
	}
	_ = dir
}

func TestAnalyzeValidExitsZero(t *testing.T) {
	dir := genAll(t)
	code, out, _ := runCLI("analyze", "--input", filepath.Join(dir, "valid.mp4"),
		"--ladder", "300000,600000", "--segment", "2s", "--recover")
	if code != report.ExitOK {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	for _, want := range []string{
		"keyframe_gap_ms=1000", "alignment_ms=0", "target_bps=600000",
		"quality_valid_samples=90", "damage_offset=-1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestAnalyzeTruncatedExitsTwo(t *testing.T) {
	dir := genAll(t)
	code, out, _ := runCLI("analyze", "--input", filepath.Join(dir, "truncated.mp4"),
		"--ladder", "300000,600000", "--segment", "2s", "--recover")
	if code != report.ExitRecovered {
		t.Fatalf("exit=%d want 2\n%s", code, out)
	}
	if !strings.Contains(out, "damage truncated") {
		t.Fatalf("missing damage line:\n%s", out)
	}
	if !strings.Contains(out, "recovered_samples=") {
		t.Fatalf("missing recovered sample total:\n%s", out)
	}
}

func TestAnalyzeCorruptExitsTwo(t *testing.T) {
	dir := genAll(t)
	code, out, _ := runCLI("analyze", "--input", filepath.Join(dir, "corrupt.mp4"),
		"--ladder", "300000,600000", "--segment", "2s", "--recover", "--json")
	if code != report.ExitRecovered {
		t.Fatalf("exit=%d want 2\n%s", code, out)
	}
	var summary report.Summary
	if err := json.Unmarshal([]byte(out), &summary); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if summary.DamageOffset < 0 {
		t.Fatalf("damage offset=%d", summary.DamageOffset)
	}
	if summary.RecoveredSamples == 0 {
		t.Fatal("no recovered samples reported")
	}
	if summary.BitrateDelta == 0 {
		t.Fatal("bitrate delta should be non-zero for this fixture")
	}
}

func TestAnalyzeBoundaryMismatchRejectsSwitch(t *testing.T) {
	dir := genAll(t)
	code, out, _ := runCLI("analyze", "--input", filepath.Join(dir, "boundary-mismatch.mp4"),
		"--ladder", "300000,600000", "--segment", "2s", "--recover", "--json")
	if code != report.ExitRecovered {
		t.Fatalf("exit=%d want 2", code)
	}
	var summary report.Summary
	if err := json.Unmarshal([]byte(out), &summary); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if summary.Manifest == nil || summary.Manifest.Switchable {
		t.Fatal("boundary mismatch left the manifest switchable")
	}
	if summary.Boundary == nil || summary.Boundary.Accepted {
		t.Fatal("boundary accepted despite mismatch")
	}
}

func TestArgumentErrorsExitOne(t *testing.T) {
	dir := genAll(t)
	cases := [][]string{
		{"analyze"},
		{"analyze", "--input", filepath.Join(dir, "valid.mp4"), "--ladder", "abc"},
		{"analyze", "--input", filepath.Join(dir, "valid.mp4"), "--ladder", "0"},
		{"analyze", "--input", filepath.Join(dir, "valid.mp4"), "--segment", "nope"},
		{"analyze", "--input", filepath.Join(dir, "valid.mp4"), "--tolerance", "-3"},
		{"analyze", "--input", filepath.Join(dir, "missing.mp4")},
		{"bogus"},
	}
	for _, args := range cases {
		if code, out, errs := runCLI(args...); code != report.ExitFailure {
			t.Fatalf("%v exit=%d want 1 (out=%q err=%q)", args, code, out, errs)
		}
	}
}

func TestJSONIsDeterministic(t *testing.T) {
	dir := genAll(t)
	path := filepath.Join(dir, "truncated.mp4")
	_, a, _ := runCLI("analyze", "--input", path, "--ladder", "300000,600000", "--segment", "2s", "--recover", "--json")
	_, b, _ := runCLI("analyze", "--input", path, "--ladder", "300000,600000", "--segment", "2s", "--recover", "--json")
	if a != b {
		t.Fatal("JSON output differs between identical runs")
	}
}

func TestMissingIndexAnalyzes(t *testing.T) {
	dir := genAll(t)
	code, out, _ := runCLI("analyze", "--input", filepath.Join(dir, "missing-index.mp4"),
		"--ladder", "300000", "--segment", "1s", "--recover")
	if code == report.ExitFailure {
		t.Fatalf("missing-index should not fail: %s", out)
	}
	if !strings.Contains(out, "index_complete=false") {
		t.Fatalf("missing-index not reported:\n%s", out)
	}
}

func TestParseDuration(t *testing.T) {
	cases := map[string]int64{
		"2s":    2000,
		"500ms": 500,
		"2000":  2000,
		"1.5s":  1500,
	}
	for in, want := range cases {
		got, err := parseDuration(in)
		if err != nil || got != want {
			t.Fatalf("parseDuration(%q)=%d,%v want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "0", "-1s"} {
		if _, err := parseDuration(bad); err == nil {
			t.Fatalf("parseDuration(%q) should fail", bad)
		}
	}
}

func TestParseLadder(t *testing.T) {
	got, err := parseLadder("300000, 600000")
	if err != nil || len(got) != 2 || got[0] != 300000 || got[1] != 600000 {
		t.Fatalf("parseLadder=%v,%v", got, err)
	}
	for _, bad := range []string{"", "abc", ",-5", "0"} {
		if _, err := parseLadder(bad); err == nil {
			t.Fatalf("parseLadder(%q) should fail", bad)
		}
	}
}
