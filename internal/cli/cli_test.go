package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUsageExitCode(t *testing.T) {
	if code := Run(nil); code != ExitBadUsage {
		t.Fatalf("no args exit = %d, want 2", code)
	}
	if code := Run([]string{"frobnicate"}); code != ExitBadUsage {
		t.Fatalf("unknown command exit = %d, want 2", code)
	}
	if code := Run([]string{"help"}); code != ExitOK {
		t.Fatalf("help exit = %d, want 0", code)
	}
	if code := Run([]string{"demo"}); code != ExitBadUsage {
		t.Fatalf("demo without --out exit = %d, want 2", code)
	}
}

func TestDemoAndResumeLifecycle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "demo")

	// Demo scans self-generated fixtures; damaged inputs yield exit 1.
	code := Run([]string{"demo", "--out", dir})
	if code != ExitDamaged {
		t.Fatalf("demo with truncated fixture exit = %d, want 1", code)
	}
	if _, err := os.Stat(filepath.Join(dir, manifestName)); err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "checkpoint.json")); err != nil {
		t.Fatalf("checkpoint not written: %v", err)
	}

	// Build a fresh scan directory sharing the same inputs/manifest, then
	// interrupt it after two segments. The manifest must exist before resume.
	interruptDir := filepath.Join(t.TempDir(), "interrupt")
	if err := os.MkdirAll(interruptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mf, err := loadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(interruptDir, mf.Params, mf.Inputs); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"resume", "--dir", interruptDir, "--interrupt-after", "2"}); code != ExitOK {
		t.Fatalf("interrupted resume exit = %d, want 0", code)
	}
	// Completing the interrupted scan without a limit processes the rest and
	// must end with the damage exit code (1), report still complete.
	if code := Run([]string{"resume", "--dir", interruptDir}); code != ExitDamaged {
		t.Fatalf("completing resume over damaged inputs exit = %d, want 1", code)
	}

	// Resuming the already-complete dir is a no-op scan and still reports
	// damage via exit 1, with an intact manifest and checkpoint.
	if code := Run([]string{"resume", "--dir", dir}); code != ExitDamaged {
		t.Fatalf("completed resume exit = %d, want 1", code)
	}
}

func TestResumeMissingDir(t *testing.T) {
	code := Run([]string{"resume", "--dir", filepath.Join(t.TempDir(), "absent")})
	if code != ExitBadUsage {
		t.Fatalf("resume missing dir exit = %d, want 2", code)
	}
}
