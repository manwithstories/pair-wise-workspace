package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"archive.local/recover/internal/report"
)

func run(args ...string) (stdout, stderr string, code int) {
	var out, errb bytes.Buffer
	code = realMain(args, &out, &errb)
	return out.String(), errb.String(), code
}

func TestNoArgsIsUsage(t *testing.T) {
	_, _, code := run()
	if code != exitUsage {
		t.Fatalf("code=%d want %d", code, exitUsage)
	}
}

func TestUnknownCommand(t *testing.T) {
	_, stderr, code := run("frobnicate")
	if code != exitUsage || !strings.Contains(stderr, "unknown command") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestHelpSucceeds(t *testing.T) {
	stdout, _, code := run("--help")
	if code != exitOK || !strings.Contains(stdout, "archive demo") {
		t.Fatalf("code=%d stdout=%q", code, stdout)
	}
}

func TestDemoSuccessJSON(t *testing.T) {
	stdout, stderr, code := run("demo", "--seed", "3", "--duration", "10",
		"--bitrates", "800,1600", "--loss", "0.03", "--json")
	if code != exitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	var rep report.Report
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, stdout)
	}
	m := rep.Metrics
	if m.SyncErrorMs > 20 || m.LossRecoveryRate < 0.9 || m.P95LatencyMs > 500 ||
		m.Segments <= 0 || m.AlignmentDeltaMs > 40 {
		t.Fatalf("acceptance metrics out of range: %+v", m)
	}
	if !rep.OK || rep.ExitCode != 0 {
		t.Fatalf("ok=%v exit=%d", rep.OK, rep.ExitCode)
	}
}

func TestDemoCorruptIndexExitThree(t *testing.T) {
	stdout, stderr, code := run("demo", "--case", "corrupt-index", "--duration", "20", "--json")
	if code != exitPartial {
		t.Fatalf("code=%d want %d (stderr=%s)", code, exitPartial, stderr)
	}
	var rep report.Report
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.OK || rep.ExitCode != exitPartial {
		t.Fatalf("ok=%v exit=%d should agree with process code", rep.OK, rep.ExitCode)
	}
	if rep.Index == nil || rep.Index.Dropped == 0 {
		t.Fatalf("index problem not reported: %+v", rep.Index)
	}
}

func TestDemoHumanOutput(t *testing.T) {
	stdout, _, code := run("demo", "--seed", "1", "--duration", "8", "--bitrates", "800")
	if code != exitOK {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{"sync_error_ms:", "loss_recovery_rate:", "p95_latency_ms:",
		"segments:", "alignment_delta_ms:"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("missing %q in output:\n%s", want, stdout)
		}
	}
}

func TestDemoRejectsBadFlags(t *testing.T) {
	cases := [][]string{
		{"demo", "--duration", "0"},
		{"demo", "--loss", "-0.1"},
		{"demo", "--loss", "1"},
		{"demo", "--case", "nonsense"},
		{"demo", "--bitrates", "abc"},
		{"demo", "--bitrates", "800,800,0"},
		{"demo", "--seed", "-1"},
		{"demo", "extra-positional"},
	}
	for _, c := range cases {
		_, _, code := run(c...)
		if code != exitUsage {
			t.Errorf("args %v: code=%d want %d", c, code, exitUsage)
		}
	}
}

func TestDemoOutDirAndResume(t *testing.T) {
	dir := t.TempDir()
	_, stderr, code := run("demo", "--seed", "9", "--duration", "10", "--bitrates", "800",
		"--json", "--out", dir)
	if code != exitOK {
		t.Fatalf("first run code=%d stderr=%s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "resume.state")); err != nil {
		t.Fatalf("resume state missing: %v", err)
	}

	stdout, stderr, code := run("demo", "--seed", "9", "--duration", "10", "--bitrates", "800",
		"--json", "--out", dir, "--resume")
	if code != exitOK {
		t.Fatalf("resume code=%d stderr=%s", code, stderr)
	}
	var rep report.Report
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Resume == nil || !rep.Resume.Used {
		t.Fatalf("resume not reported as used: %+v", rep.Resume)
	}
}

func TestResumeRefusesChangedConfig(t *testing.T) {
	dir := t.TempDir()
	if _, _, code := run("demo", "--seed", "9", "--duration", "10", "--bitrates", "800", "--json", "--out", dir); code != exitOK {
		t.Fatal("setup run failed")
	}
	stdout, stderr, code := run("demo", "--seed", "9", "--duration", "10", "--bitrates", "1600",
		"--json", "--out", dir, "--resume")
	if code != exitUnrecoverable {
		t.Fatalf("code=%d want %d", code, exitUnrecoverable)
	}
	if !strings.Contains(stderr, "refusing") {
		t.Fatalf("stderr=%q", stderr)
	}
	var rec struct {
		OK     bool `json:"ok"`
		Issues []struct {
			Code string `json:"code"`
		} `json:"issues"`
	}
	if err := json.Unmarshal([]byte(stdout), &rec); err != nil {
		t.Fatalf("refusal is not stable JSON: %v\n%s", err, stdout)
	}
	if rec.OK || len(rec.Issues) == 0 {
		t.Fatalf("refusal record incomplete: %s", stdout)
	}
	code = 0
	for _, i := range rec.Issues {
		if strings.HasPrefix(i.Code, "resume.") {
			code = 1
		}
	}
	if code == 0 {
		t.Fatalf("refusal does not classify the digest mismatch: %s", stdout)
	}
}

func TestVerifyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, _, code := run("demo", "--seed", "4", "--duration", "10", "--bitrates", "800", "--json", "--out", dir); code != exitOK {
		t.Fatal("setup run failed")
	}
	stdout, stderr, code := run("verify", "--dir", dir, "--json")
	if code != exitOK {
		t.Fatalf("verify code=%d stderr=%s", code, stderr)
	}
	var rep report.Report
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Metrics.Segments <= 0 {
		t.Fatalf("verified manifest lost its metrics: %+v", rep.Metrics)
	}
}

func TestVerifyMissingDir(t *testing.T) {
	_, _, code := run("verify")
	if code != exitUsage {
		t.Fatalf("code=%d", code)
	}
	_, _, code = run("verify", "--dir", t.TempDir())
	if code == exitOK {
		t.Fatal("verify of empty dir succeeded")
	}
}
