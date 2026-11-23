package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildBinary compiles the command once for exit-code tests.
func buildBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "archive")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Env = os.Environ()
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, b)
	}
	return out
}

func TestDemoExitCodeDamaged(t *testing.T) {
	bin := buildBinary(t)
	work := t.TempDir()
	cmd := exec.Command(bin, "demo", "--seed", "41", "--duration", "60s",
		"--corrupt-at", "184320", "--interrupt-after", "1200", "--resume",
		"--workdir", work)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := cmd.ProcessState.ExitCode()
	// A damaged source must yield exit code 2.
	if code != 2 {
		t.Fatalf("exit = %d (err=%v)\nstdout:\n%s\nstderr:\n%s", code, err, stdout.String(), stderr.String())
	}
	fields := map[string]bool{}
	for _, line := range strings.Split(stdout.String(), "\n") {
		if f := strings.SplitN(line, " ", 2); len(f) == 2 {
			fields[strings.TrimSpace(f[0])] = true
		}
	}
	for _, want := range []string{"damage_offset_bytes", "recovered_samples", "resume_ms",
		"duplicate_packets", "reordered_packets", "burst_loss_packets", "cache_rejected",
		"index_sha256"} {
		if !fields[want] {
			t.Errorf("missing field %q in output:\n%s", want, stdout.String())
		}
	}
}

func TestDemoHealthyExitCodeZero(t *testing.T) {
	bin := buildBinary(t)
	work := t.TempDir()
	cmd := exec.Command(bin, "demo", "--seed", "41", "--duration", "30s",
		"--corrupt-at", "0", "--workdir", work)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := cmd.ProcessState.ExitCode()
	if code != 0 {
		t.Fatalf("exit = %d (err=%v)\nstdout:\n%s\nstderr:\n%s", code, err, stdout.String(), stderr.String())
	}
}

func TestUnknownCommandFails(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin, "bogus")
	err := cmd.Run()
	if cmd.ProcessState.ExitCode() != 1 {
		t.Fatalf("exit = %d (err=%v)", cmd.ProcessState.ExitCode(), err)
	}
}
