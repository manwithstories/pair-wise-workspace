package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func demoConfig(dir string) config {
	return config{
		seed: 41, lossBurst: 7, bandwidth: []float64{3, 1, 6},
		fixtureCase: "normal", workDir: dir, fragments: 8,
	}
}

// capture runs f with stdout redirected and returns what it printed.
func capture(t *testing.T, f func() int) (string, int) {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code := f()
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out), code
}

func field(out, key string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, key+"=") {
			return strings.TrimPrefix(line, key+"=")
		}
	}
	return ""
}

func TestDemoNormalRunPasses(t *testing.T) {
	out, code := capture(t, func() int { return runDemo(demoConfig(t.TempDir())) })
	if code != exitOK {
		t.Fatalf("exit=%d want 0:\n%s", code, out)
	}
	for _, key := range []string{"av_sync_ms", "loss_recovery_rate", "latency_p95_ms",
		"duplicate_packets", "timeline_rollbacks", "committed_segments", "fixture_case"} {
		if field(out, key) == "" {
			t.Fatalf("required field %s missing:\n%s", key, out)
		}
	}
	if field(out, "fixture_case") != "normal" {
		t.Fatalf("fixture_case=%s", field(out, "fixture_case"))
	}
}

func TestDemoCrashResumeConsistent(t *testing.T) {
	// Clean run.
	outClean, codeClean := capture(t, func() int { return runDemo(demoConfig(t.TempDir())) })
	if codeClean != exitOK {
		t.Fatalf("clean run exit=%d", codeClean)
	}
	// Crash after split, then in-process resume.
	cfg := demoConfig(t.TempDir())
	cfg.crashAfterStage = "split"
	outCrash, codeCrash := capture(t, func() int { return runDemo(cfg) })
	if codeCrash != exitOK {
		t.Fatalf("crash run exit=%d:\n%s", codeCrash, outCrash)
	}
	if field(outCrash, "committed_segments") != field(outClean, "committed_segments") {
		t.Fatalf("committed_segments differ: crash=%s clean=%s",
			field(outCrash, "committed_segments"), field(outClean, "committed_segments"))
	}
	if field(outCrash, "timeline_summary") != field(outClean, "timeline_summary") {
		t.Fatal("timeline summary differs between crash and clean run")
	}
	if field(outCrash, "recovery_ok") != "true" {
		t.Fatalf("recovery_ok=%s", field(outCrash, "recovery_ok"))
	}
}

func TestDemoResumeDoesNotRecommit(t *testing.T) {
	dir := t.TempDir()
	cfg := demoConfig(dir)
	if _, code := capture(t, func() int { return runDemo(cfg) }); code != exitOK {
		t.Fatal("first run failed")
	}
	cfg.resume = true
	out, code := capture(t, func() int { return runDemo(cfg) })
	if code != exitOK {
		t.Fatalf("resume exit=%d:\n%s", code, out)
	}
	if field(out, "commit_skipped") != "true" {
		t.Fatal("resume re-committed an already committed txn")
	}
	// Exactly one committed record in the journal.
	data, err := os.ReadFile(filepath.Join(dir, "wal.log"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), `"type":"committed"`); n != 1 {
		t.Fatalf("committed records=%d want 1", n)
	}
}

func TestDemoCorruptExits3WithPartialCommit(t *testing.T) {
	cfg := demoConfig(t.TempDir())
	cfg.fixtureCase = "corrupt"
	out, code := capture(t, func() int { return runDemo(cfg) })
	if code != exitCorrupt {
		t.Fatalf("exit=%d want 3:\n%s", code, out)
	}
	if field(out, "parse_failures") == "0" || field(out, "parse_failures") == "" {
		t.Fatal("corrupt fixtures produced no parse failures")
	}
	if field(out, "committed_segments") == "0" || field(out, "committed_segments") == "" {
		t.Fatal("valid fragments were not committed alongside corrupt ones")
	}
}

func TestArgumentValidation(t *testing.T) {
	cases := [][]string{
		{}, // no subcommand
		{"demo", "--fixture", "bogus"},
		{"demo", "--crash-after-stage", "bogus"},
		{"demo", "--bandwidth-mbps", "3,x"},
		{"demo", "--loss-burst", "-1"},
		{"demo", "--fragments", "0"},
	}
	for i, args := range cases {
		if code := run(args); code != exitArgs {
			t.Fatalf("case %d %v: exit=%d want 2", i, args, code)
		}
	}
}
