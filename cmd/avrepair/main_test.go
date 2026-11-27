package main

import (
	"encoding/json"
	"testing"

	"avrepair/internal/engine"
)

func TestDemoBatchExitCodes(t *testing.T) {
	// A batch containing the interrupted case still completes at process level
	// (0); the unrecoverable outcome is carried per-case in the JSON.
	out := t.TempDir()
	code := run([]string{"demo", "--out", out,
		"--cases", "clean,interrupted", "--json"})
	if code != engine.ExitOK {
		t.Fatalf("batch process exit=%d want 0", code)
	}

	// Argument errors map to exit 2.
	if got := run([]string{"demo"}); got != engine.ExitArgs {
		t.Errorf("missing --out exit=%d want 2", got)
	}
	if got := run([]string{"demo", "--out", out, "--cases", "bogus"}); got != engine.ExitArgs {
		t.Errorf("bad case exit=%d want 2", got)
	}
	if got := run([]string{"nope"}); got != engine.ExitArgs {
		t.Errorf("bad subcommand exit=%d want 2", got)
	}
}

func TestRepairSinglePropagatesCaseCode(t *testing.T) {
	out := t.TempDir()
	if got := run([]string{"repair", "--out", out, "--case", "interrupted"}); got != engine.ExitUnrecoverable {
		t.Errorf("interrupted repair exit=%d want 3", got)
	}
	if got := run([]string{"repair", "--out", t.TempDir(), "--case", "clean", "--rerun"}); got != engine.ExitOK {
		t.Errorf("clean repair exit=%d want 0", got)
	}
}

func TestKnownCaseAndSplit(t *testing.T) {
	if !knownCase("clean") || knownCase("missing") {
		t.Fatal("knownCase lookup wrong")
	}
	got := splitCases(" clean, rollback ,, interrupted,")
	want := []string{"clean", "rollback", "interrupted"}
	if len(got) != len(want) {
		t.Fatalf("splitCases=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitCases=%v want %v", got, want)
		}
	}
}

func TestReportJSONRoundTrip(t *testing.T) {
	r := &engine.Report{Case: "x", TargetBPS: 1, ActualBPS: 1, OutputSHA256: "abc"}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"case", "target_bps", "actual_bps", "output_sha256"} {
		if _, ok := back[key]; !ok {
			t.Errorf("JSON missing %q", key)
		}
	}
}
