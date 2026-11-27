package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"avflow/internal/model"
)

func runMain(t *testing.T, args ...string) (Report, int) {
	t.Helper()
	f, err := parseSelfcheck(args)
	if err != nil {
		return Report{}, model.ExitArgs
	}
	rep, code := execute(f)
	return *rep, code
}

func TestSelfcheckCrashResumeReport(t *testing.T) {
	work := filepath.Join(t.TempDir(), "w")
	rep, code := runMain(t,
		"--seed", "417", "--packets", "900", "--fault-set", "all",
		"--crash-after", "normalize", "--repeat", "2", "--json", "--work", work)
	if code != model.ExitSimulatedCrash {
		t.Fatalf("exit code = %d, want 75", code)
	}
	if rep.FaultPrecisionPackets > 1 {
		t.Fatalf("precision = %d, want <=1", rep.FaultPrecisionPackets)
	}
	if rep.FalsePositives != 0 {
		t.Fatalf("false positives = %d, want 0", rep.FalsePositives)
	}
	if rep.ResumeMS <= 0 {
		t.Fatal("resume time not measured")
	}
	if len(rep.SHA256) != 64 {
		t.Fatalf("sha256 length = %d", len(rep.SHA256))
	}
	if !rep.RepeatSHA256Match || !rep.RollbackOK {
		t.Fatalf("repeat=%v rollback=%v", rep.RepeatSHA256Match, rep.RollbackOK)
	}
	if rep.P95WindowMS <= 0 || rep.P95WindowMS > 120 {
		t.Fatalf("p95 %f outside (0,120]", rep.P95WindowMS)
	}
}

func TestSelfcheckCleanExitZero(t *testing.T) {
	work := filepath.Join(t.TempDir(), "w")
	rep, code := runMain(t, "--seed", "9", "--packets", "300",
		"--fault-set", "none", "--repeat", "1", "--work", work)
	if code != model.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if rep.FalsePositives != 0 || rep.FirstFault != nil {
		t.Fatalf("clean run reported defects: %+v", rep)
	}
	if !rep.RepeatSHA256Match {
		t.Fatal("clean run checksums must be stable")
	}
}

func TestSelfcheckFatalExitThree(t *testing.T) {
	work := filepath.Join(t.TempDir(), "w")
	_, code := runMain(t, "--seed", "3", "--packets", "300",
		"--fault-set", "fatal", "--work", work)
	if code != model.ExitUnrecoverable {
		t.Fatalf("exit = %d, want 3", code)
	}
}

func TestArgumentErrorsExitTwo(t *testing.T) {
	for _, args := range [][]string{
		{"--packets", "10"},
		{"--work", "/tmp/x", "--packets", "-1"},
		{"--work", "/tmp/x", "--fault-set", "nope"},
		{"--work", "/tmp/x", "--crash-after", "bogus"},
	} {
		if _, err := parseSelfcheck(args); err == nil {
			t.Fatalf("args %v accepted", args)
		}
	}
}

func TestJSONShape(t *testing.T) {
	// Marshal a report and confirm the required JSON keys are present.
	rep := Report{
		FaultPrecisionPackets: 0, FalsePositives: 0, P95WindowMS: 42.5,
		ResumeMS: 9.1, SHA256: strings.Repeat("a", 64),
		RepeatSHA256Match: true, RollbackOK: true, ExitCode: 0,
	}
	data, err := json.Marshal(&rep)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, key := range []string{
		"fault_precision_packets", "false_positives", "p95_window_ms",
		"resume_ms", "sha256", "repeat_sha256_match", "rollback_ok",
	} {
		if !strings.Contains(text, key) {
			t.Fatalf("JSON missing key %q", key)
		}
	}
}
