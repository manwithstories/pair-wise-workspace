package main

import (
	"path/filepath"
	"testing"

	"relayctl/internal/fixture"
	"relayctl/internal/pipeline"
)

// TestCLIExitCodes drives run() exactly like the terminal entry point and
// asserts the documented exit-code contract for both fault chains.
func TestCLIExitCodes(t *testing.T) {
	root := t.TempDir()

	goodIn := filepath.Join(root, "good")
	goodState := filepath.Join(root, "good-state")
	good, err := fixture.Generate(fixture.CaseDriftWrapVFR, 41)
	if err != nil {
		t.Fatal(err)
	}
	if err := pipeline.SaveScenario(goodIn, good); err != nil {
		t.Fatal(err)
	}

	if code := run([]string{"relayctl", "run", "--input", goodIn, "--state", goodState,
		"--pause-after", "sync"}); code != exitPaused {
		t.Fatalf("paused run code = %d want %d", code, exitPaused)
	}
	if code := run([]string{"relayctl", "resume", "--input", goodIn, "--state", goodState}); code != exitOK {
		t.Fatalf("resume code = %d want %d", code, exitOK)
	}
	if code := run([]string{"relayctl", "verify", "--input", goodIn, "--state", goodState,
		"--repeat", "2"}); code != exitOK {
		t.Fatalf("verify code = %d want %d", code, exitOK)
	}

	// Damage chain: truncated mdat located with exit code 2 on inspect.
	damIn := filepath.Join(root, "dam")
	dam, err := fixture.Generate(fixture.CaseTruncateMdat, 42)
	if err != nil {
		t.Fatal(err)
	}
	if err := pipeline.SaveScenario(damIn, dam); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"relayctl", "inspect", "--input", damIn}); code != exitDamaged {
		t.Fatalf("inspect code = %d want %d", code, exitDamaged)
	}
	// Burst loss on the damaged stream is unrecoverable (exit 3).
	if code := run([]string{"relayctl", "run", "--input", damIn,
		"--state", filepath.Join(root, "dam-state")}); code != exitLost {
		t.Fatalf("damaged run code = %d want %d", code, exitLost)
	}
}

func TestCLIUsageErrors(t *testing.T) {
	if code := run([]string{"relayctl"}); code != exitUsage {
		t.Fatalf("no args code = %d want %d", code, exitUsage)
	}
	if code := run([]string{"relayctl", "bogus"}); code != exitUsage {
		t.Fatalf("bad subcommand code = %d want %d", code, exitUsage)
	}
	if code := run([]string{"relayctl", "fixture", "--case", fixture.CaseDriftWrapVFR}); code != exitUsage {
		t.Fatalf("missing required flag code = %d want %d", code, exitUsage)
	}
	if code := run([]string{"relayctl", "fixture", "--case", "nope", "--seed", "1",
		"--out", filepath.Join(t.TempDir(), "x")}); code != exitUsage {
		t.Fatalf("unknown case code = %d want %d", code, exitUsage)
	}
}

func TestCLIHelp(t *testing.T) {
	if code := run([]string{"relayctl", "help"}); code != exitOK {
		t.Fatalf("help code = %d want %d", code, exitOK)
	}
}
