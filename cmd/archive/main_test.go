package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"archive/internal/metrics"
	"archive/internal/rtp"
)

// The command adapter is tested through run(): assertions are on exit
// codes, printed contract lines and on-disk artifacts.

func demoArgs(root string, extra ...string) []string {
	return append([]string{"demo", "--root", root, "--segments", "6"}, extra...)
}

func TestDemoCompleteExitZero(t *testing.T) {
	root := t.TempDir()
	if code := run(demoArgs(root, "--interrupt-after", "-1")); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{"media.m3u8", "manifest.mpd", "work/metrics.json"} {
		if _, err := os.Stat(filepath.Join(root, want)); err != nil {
			t.Fatalf("missing %s: %v", want, err)
		}
	}
}

func TestDemoInterruptThenResume(t *testing.T) {
	root := t.TempDir()
	if code := run(demoArgs(root, "--interrupt-after", "2")); code != 0 {
		t.Fatalf("demo exit = %d", code)
	}
	if code := run([]string{"resume", "--root", root}); code != 0 {
		t.Fatalf("resume exit = %d", code)
	}
	// A second resume must be a no-op that still succeeds.
	if code := run([]string{"resume", "--root", root}); code != 0 {
		t.Fatalf("second resume exit = %d", code)
	}
}

func TestDemoTruncatedExitTwo(t *testing.T) {
	root := t.TempDir()
	if code := run(demoArgs(root, "--corrupt", "truncated")); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}

func TestDemoMissingExitTwo(t *testing.T) {
	root := t.TempDir()
	if code := run(demoArgs(root, "--corrupt", "missing")); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}

func TestDemoConflictExitOne(t *testing.T) {
	root := t.TempDir()
	if code := run(demoArgs(root, "--corrupt", "conflict")); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
}

func TestDemoWrapAndDuplicateSucceed(t *testing.T) {
	for _, kind := range []string{"wrap", "duplicate"} {
		root := t.TempDir()
		if code := run(demoArgs(root, "--corrupt", kind)); code != 0 {
			t.Fatalf("%s exit = %d, want 0", kind, code)
		}
	}
}

func TestDemoRequiresRoot(t *testing.T) {
	if code := run([]string{"demo"}); code != 1 {
		t.Fatalf("missing --root exit = %d, want 1", code)
	}
}

func TestUnknownCommand(t *testing.T) {
	if code := run([]string{"frobnicate"}); code != 1 {
		t.Fatalf("unknown command exit = %d", code)
	}
}

func TestResumeRequiresExistingManifest(t *testing.T) {
	if code := run([]string{"resume", "--root", t.TempDir()}); code != 1 {
		t.Fatalf("empty root exit = %d want 1", code)
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns
// everything it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	os.Stdout = old
	w.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestReportLineContract(t *testing.T) {
	out := captureStdout(t, func() {
		printReport(metrics.Report{
			SyncErrorMs: 0.4, P95LatencyMs: 24.0, CacheVerified: 120,
			OutputSHA: strings.Repeat("a", 64),
		}, rtp.Result{Sent: 120, Received: 121, Recovered: 3, Rate: 100})
	})
	for _, want := range []string{
		"sync_error_ms=0.4", "recovered_packets=3", "recovery_rate=100.0",
		"p95_latency_ms=24.0", "cache_verified=120",
		"output_sha256=" + strings.Repeat("a", 64),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report line lacks %q:\n%s", want, out)
		}
	}
}

func TestBadSegmentLineContract(t *testing.T) {
	root := t.TempDir()
	var out string
	withDemo := func() { run(demoArgs(root, "--corrupt", "truncated")) }
	out = captureStdout(t, withDemo)
	if !strings.Contains(out, "first_bad_segment=media/seg-00003.m4s") {
		t.Fatalf("bad segment line missing:\n%s", out)
	}
	if !strings.Contains(out, "recoverable_segments=3") {
		t.Fatalf("recoverable count missing:\n%s", out)
	}
}

func TestCorruptOptionsPlaceDamageMidStream(t *testing.T) {
	o, err := corruptOptions("truncated", 120)
	if err != nil {
		t.Fatal(err)
	}
	if o.TruncateAt != 60 {
		t.Fatalf("TruncateAt = %d, want 60", o.TruncateAt)
	}
	if _, err := corruptOptions("bogus", 10); err == nil {
		t.Fatal("unknown kind must be rejected")
	}
}

func TestRTPPlanKeepsDrillDeterministic(t *testing.T) {
	n, inj := rtpPlan(120, 37)
	if n != 120 || inj.DropEvery != 37 || inj.DuplicateAt != 1 || inj.ReorderEvery == 0 {
		t.Fatalf("plan = %d %+v", n, inj)
	}
	n, inj = rtpPlan(5000, 0)
	if n != 200 || inj.DropEvery != 0 {
		t.Fatalf("capped plan = %d %+v", n, inj)
	}
}

func TestReadmeDocumentsContract(t *testing.T) {
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"sync_error_ms", "recovered_packets", "recovery_rate",
		"p95_latency_ms", "cache_verified", "output_sha256",
		"first_bad_segment", "recoverable_segments",
	} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("README does not document %q", want)
		}
	}
	if strings.Contains(string(raw), "\r\n") {
		t.Error("README must use Unix line endings")
	}
}
