package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"segmanifest/internal/manifest"
)

func runCommand(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestAcceptanceFlow(t *testing.T) {
	root := t.TempDir()
	setA := filepath.Join(root, "testdata", "setA")
	manifestPath := filepath.Join(root, "out", "setA.smf")
	pathsPath := filepath.Join(root, "out", "paths.txt")
	if code, _, stderr := runCommand(t, "gen", "--out", setA, "--seed", "7", "--representations", "3", "--segments", "120", "--offset-ms", "-37", "--jitter-ms", "1.6"); code != 0 {
		t.Fatalf("gen code=%d stderr=%s", code, stderr)
	}
	code, stdout, stderr := runCommand(t, "plan", "--in", setA, "--out", manifestPath, "--target", "4s", "--tolerance", "2ms")
	if code != 0 {
		t.Fatalf("plan code=%d stderr=%s", code, stderr)
	}
	wantPlan := "representations=3 segments=360 grid_points=121 max_drift_ms=1.43 p95_drift_ms=0.92 resync_points=0 discontinuities=0\n"
	if stdout != wantPlan {
		t.Fatalf("plan output=%q", stdout)
	}
	if code, _, stderr := runCommand(t, "emit", "--manifest", manifestPath, "--rep", "r1080", "--out", pathsPath); code != 0 {
		t.Fatalf("emit code=%d stderr=%s", code, stderr)
	}
	data, err := os.ReadFile(pathsPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 120 || lines[0] != "seg/r1080/00001.m4s" {
		t.Fatalf("paths count=%d first=%q", len(lines), lines[0])
	}
	code, stdout, stderr = runCommand(t, "verify", "--manifest", manifestPath, "--json")
	if code != 0 {
		t.Fatalf("verify code=%d stderr=%s", code, stderr)
	}
	var result struct {
		ExpandedPaths  int     `json:"expanded_paths"`
		DuplicatePaths int     `json:"duplicate_paths"`
		Holes          int     `json:"holes"`
		OverlapMS      float64 `json:"overlap_ms"`
		RoundtripOK    bool    `json:"roundtrip_ok"`
		OK             bool    `json:"ok"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if result.ExpandedPaths != 360 || result.DuplicatePaths != 0 || result.Holes != 0 || result.OverlapMS != 0 || !result.RoundtripOK || !result.OK {
		t.Fatalf("verify=%+v", result)
	}
}

func TestPlanIsByteDeterministic(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "fixture")
	if code, _, stderr := runCommand(t, "gen", "--out", source, "--seed", "9", "--segments", "20", "--jitter-ms", "1.2"); code != 0 {
		t.Fatalf("gen: %d %s", code, stderr)
	}
	first, second := filepath.Join(root, "first.smf"), filepath.Join(root, "second.smf")
	for _, path := range []string{first, second} {
		if code, _, stderr := runCommand(t, "plan", "--in", source, "--out", path, "--target", "4s", "--tolerance", "2ms"); code != 0 {
			t.Fatalf("plan: %d %s", code, stderr)
		}
	}
	a, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("identical plan runs produced different bytes")
	}
}

func TestWindowRollingKeepsNumbers(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "fixture")
	out := filepath.Join(root, "window.smf")
	if code, _, stderr := runCommand(t, "gen", "--out", source, "--segments", "20"); code != 0 {
		t.Fatalf("gen: %d %s", code, stderr)
	}
	if code, _, stderr := runCommand(t, "plan", "--in", source, "--out", out, "--window", "5"); code != 0 {
		t.Fatalf("plan: %d %s", code, stderr)
	}
	file, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	m, err := manifest.Decode(file)
	if err != nil {
		t.Fatal(err)
	}
	if m.Window.Version != 2 || m.Window.StartNumber != 16 || len(m.Representations[0].Segments) != 5 {
		t.Fatalf("window=%+v segments=%d", m.Window, len(m.Representations[0].Segments))
	}
	if m.Representations[0].Segments[0].Number != 16 {
		t.Fatalf("first number=%d", m.Representations[0].Segments[0].Number)
	}
}

func TestExitCodes(t *testing.T) {
	root := t.TempDir()
	bad := filepath.Join(root, "bad.smf")
	if err := os.WriteFile(bad, []byte("not a manifest"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runCommand(t, "verify", "--manifest", bad); code != exitDecode {
		t.Fatalf("bad manifest exit=%d", code)
	}
	if code, _, _ := runCommand(t, "plan", "--in", root, "--out", filepath.Join(root, "x")); code != exitInput {
		t.Fatalf("bad input exit=%d", code)
	}
	if code, _, _ := runCommand(t, "gen", "--out", filepath.Join(root, "strict"), "--segments", "12", "--jitter-ms", "5"); code != 0 {
		t.Fatalf("strict fixture exit=%d", code)
	}
	strictOut := filepath.Join(root, "strict.smf")
	if code, _, stderr := runCommand(t, "plan", "--in", filepath.Join(root, "strict"), "--out", strictOut, "--tolerance", "2ms"); code != 0 {
		t.Fatalf("strict plan exit=%d %s", code, stderr)
	}
	if code, _, _ := runCommand(t, "verify", "--manifest", strictOut, "--strict"); code != exitVerify {
		t.Fatalf("strict verify exit=%d", code)
	}
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runCommand(t, "stats", "--manifest", strictOut); code != 0 {
		t.Fatalf("stats exit=%d", code)
	}
	if code, _, _ := runCommand(t, "plan", "--in", filepath.Join(root, "strict"), "--out", filepath.Join(blocker, "out.smf")); code != exitSubtask {
		t.Fatalf("subtask failure exit=%d", code)
	}
}
