package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestUsageAndUnknownCommand(t *testing.T) {
	code, out, _ := runCLI(t, "help")
	if code != 0 || !strings.Contains(out, "mediacheck demo") {
		t.Fatalf("help: code=%d out=%q", code, out)
	}
	code, _, errOut := runCLI(t, "explode")
	if code != 1 || !strings.Contains(errOut, "unknown command") {
		t.Fatalf("unknown command: code=%d err=%q", code, errOut)
	}
	code, _, _ = runCLI(t)
	if code != 1 {
		t.Fatalf("no args exit = %d, want 1", code)
	}
}

func TestDemoCleanExitsZeroWithAllFields(t *testing.T) {
	code, out, errOut := runCLI(t, "demo", "--profile", "clean", "--seed", "17",
		"--packets", "300", "--queue", "8")
	if code != 0 {
		t.Fatalf("clean exit = %d\nstderr=%s", code, errOut)
	}
	for _, key := range []string{
		"sync_error_ms=", "loss_recovery_pct=", "p95_ms=", "queue_peak=",
		"damaged_ranges=", "output_sha256=",
	} {
		if !strings.Contains(out, key) {
			t.Fatalf("missing %q in output:\n%s", key, out)
		}
	}
	if !strings.Contains(out, "targets_met=true") {
		t.Fatalf("clean run missed targets:\n%s", out)
	}
	if !strings.Contains(out, "queue_peak=8") {
		t.Fatalf("queue peak did not hit the bound:\n%s", out)
	}
}

func TestDemoDamagedExitsTwoAndWritesOutput(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "recovered.mkv")
	code, out, errOut := runCLI(t, "demo", "--profile", "damaged", "--seed", "17",
		"--packets", "400", "--queue", "16", "--output", dest)
	if code != 2 {
		t.Fatalf("damaged exit = %d, want 2\nstderr=%s", code, errOut)
	}
	if !strings.Contains(out, "loss_recovery_pct=100.00") {
		t.Fatalf("recovery not complete:\n%s", out)
	}
	if strings.Contains(out, "output_sha256=none") {
		t.Fatalf("output hash missing:\n%s", out)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("output not written: %v", err)
	}
	if len(data) < 4 || data[0] != 0x1A {
		t.Fatal("output is not Matroska")
	}
}

func TestDemoOutputIsReproducible(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.mkv")
	b := filepath.Join(dir, "b.mkv")
	_, outA, _ := runCLI(t, "demo", "--profile", "damaged", "--seed", "17",
		"--packets", "300", "--queue", "8", "--output", a)
	_, outB, _ := runCLI(t, "demo", "--profile", "damaged", "--seed", "17",
		"--packets", "300", "--queue", "8", "--output", b)
	if hashOf(t, outA) != hashOf(t, outB) {
		t.Fatalf("hashes differ between identical runs:\n%s\n%s", outA, outB)
	}
}

func hashOf(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "output_sha256=") {
			return strings.TrimPrefix(line, "output_sha256=")
		}
	}
	t.Fatalf("no output_sha256 in %q", out)
	return ""
}

func TestDemoRejectsBadFlags(t *testing.T) {
	for _, args := range [][]string{
		{"demo", "--profile", "chaos"},
		{"demo", "--packets", "0"},
		{"demo", "--queue", "0"},
		{"demo", "--seed", "-1"},
		{"demo", "--codec", "vp9"},
		{"demo", "--slow-consumer", "-1ms"},
		{"demo", "--frames", "-5"},
	} {
		code, _, errOut := runCLI(t, args...)
		if code != 1 {
			t.Fatalf("%v exit = %d, want 1", args, code)
		}
		if errOut == "" {
			t.Fatalf("%v produced no diagnostic", args)
		}
	}
}

func TestDemoJSONFlag(t *testing.T) {
	code, out, _ := runCLI(t, "demo", "--profile", "clean", "--packets", "120",
		"--queue", "4", "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, `"sync_error_ms"`) || !strings.Contains(out, `"output_sha256"`) {
		t.Fatalf("JSON block missing fields:\n%s", out)
	}
}

func TestDemoMixedDurations(t *testing.T) {
	code, out, _ := runCLI(t, "demo", "--profile", "clean", "--packets", "300",
		"--queue", "8", "--mixed-durations")
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if !strings.Contains(out, "packets=") {
		t.Fatal("no packet count")
	}
}

func TestDemoTruncatedAndWrapProfiles(t *testing.T) {
	for _, profile := range []string{"truncated", "wrap"} {
		code, out, errOut := runCLI(t, "demo", "--profile", profile, "--packets", "300", "--queue", "8")
		if code != 2 {
			t.Fatalf("%s exit = %d, want 2\nstderr=%s", profile, code, errOut)
		}
		if !strings.Contains(out, "damaged_ranges=") || strings.Contains(out, "damaged_ranges=\n") {
			t.Fatalf("%s reported no damaged ranges:\n%s", profile, out)
		}
	}
}
