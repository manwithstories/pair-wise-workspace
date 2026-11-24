package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"mediaflow/internal/pipeline"
)

func runArgs(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestDemoExitZeroOnDefault(t *testing.T) {
	code, out, errOut := runArgs(t, "demo", "--seed", "7", "--representations", "3",
		"--loss", "0.03", "--truncate-every", "11", "--compact")
	if code != 0 {
		t.Fatalf("exit = %d stderr=%s", code, errOut)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	for _, k := range []string{"sync_error_ms", "loss_recovery_rate", "p95_latency_ms",
		"damaged_ranges", "gop_interval_ms", "boundary_mismatch_count"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing %q in output", k)
		}
	}
	if m["pass"] != true {
		t.Fatalf("pass = %v", m["pass"])
	}
}

func TestInvalidArgumentsExitTwo(t *testing.T) {
	cases := [][]string{
		{},
		{"bogus"},
		{"demo", "--representations", "0"},
		{"demo", "--loss", "2"},
		{"demo", "--truncate-every", "-1"},
		{"demo", "--window", "1"},
		{"demo", "extra-positional"},
		{"demo", "--nope"},
	}
	for _, c := range cases {
		code, _, _ := runArgs(t, c...)
		if code != 2 {
			t.Fatalf("args %v: exit = %d, want 2", c, code)
		}
	}
}

func TestVerifyPrintsVerdict(t *testing.T) {
	code, out, _ := runArgs(t, "verify")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "PASS") || !strings.Contains(out, "sync_error_ms=") {
		t.Fatalf("verdict output = %q", out)
	}
}

func TestThresholdFailureExitOne(t *testing.T) {
	// Heavy loss drives the recovery rate below the accepted floor.
	code, out, _ := runArgs(t, "verify", "--loss", "0.5")
	if code != 1 {
		t.Fatalf("exit = %d, want 1; output=%s", code, out)
	}
	if !strings.Contains(out, "FAIL") {
		t.Fatalf("expected FAIL verdict, got %q", out)
	}
}

func TestDemoIsReproducible(t *testing.T) {
	_, a, _ := runArgs(t, "demo", "--seed", "7", "--compact")
	_, b, _ := runArgs(t, "demo", "--seed", "7", "--compact")
	var ma, mb pipeline.Summary
	if err := json.Unmarshal([]byte(a), &ma); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(b), &mb); err != nil {
		t.Fatal(err)
	}
	if ma.DeterministicDigest != mb.DeterministicDigest {
		t.Fatalf("digest drift %s vs %s", ma.DeterministicDigest, mb.DeterministicDigest)
	}
	if len(ma.DamagedRanges) != len(mb.DamagedRanges) {
		t.Fatalf("damage count drift")
	}
}
