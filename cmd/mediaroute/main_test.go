package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mediaroute/internal/fixture"
)

// buildCLI compiles the mediaroute binary once into the test temp dir.
func buildCLI(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "mediaroute")
	cmd := exec.Command(goBin(), "build", "-o", binary, "./cmd/mediaroute")
	cmd.Dir = repoRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build cli: %v\n%s", err, out)
	}
	return binary
}

func goBin() string {
	if p := lookGo(); p != "" {
		return p
	}
	return "go"
}

func lookGo() string {
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	return ""
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	// cmd/mediaroute/main_test.go -> repository root is two levels up.
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func runCLI(t *testing.T, binary string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	out, _ := cmd.CombinedOutput()
	return string(out), cmd.ProcessState.ExitCode()
}

func TestCLIDemoSuccess(t *testing.T) {
	bin := buildCLI(t)
	out, code := runCLI(t, bin, "demo", "--seed", "41",
		"--drop-burst", "8", "--duplicate", "3",
		"--reorder-window", "12", "--bandwidth-mbps", "6,1,9",
		"--interrupt-after", "120", "--resume")
	if code != 0 {
		t.Fatalf("demo exit=%d output=%s", code, out)
	}
	for _, key := range []string{
		"scan.samples", "keyframe.interval_p95_ms",
		"damage.offset_error_samples", "resume.elapsed_ms",
		"resume.replayed_samples", "output.sha256",
	} {
		if !strings.Contains(out, key) {
			t.Fatalf("output missing %q:\n%s", key, out)
		}
	}
}

func TestCLIBadArgumentExit4(t *testing.T) {
	bin := buildCLI(t)
	if _, code := runCLI(t, bin, "demo", "--bandwidth-mbps", "6,x"); code != 4 {
		t.Fatalf("bad bandwidth exit=%d want 4", code)
	}
	if _, code := runCLI(t, bin); code != 4 {
		t.Fatalf("no subcommand exit=%d want 4", code)
	}
}

func TestCLIScanCorruptExit2(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	spec := fixture.DefaultSpec(41)
	spec.TruncateMDAT = true
	raw, err := spec.BuildMP4()
	if err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "corrupt.mp4")
	if err := os.WriteFile(bad, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	out, code := runCLI(t, bin, "scan", "--input", bad,
		"--index-dir", filepath.Join(dir, "idx"))
	if code != 2 {
		t.Fatalf("corrupt scan exit=%d want 2, output=%s", code, out)
	}
	if !strings.Contains(out, "truncated_sample_table") {
		t.Fatalf("output missing fault category: %s", out)
	}
}

func TestCLIHelp(t *testing.T) {
	bin := buildCLI(t)
	out, code := runCLI(t, bin, "help")
	if code != 0 || !strings.Contains(out, "mediaroute") {
		t.Fatalf("help failed: code=%d out=%s", code, out)
	}
}
