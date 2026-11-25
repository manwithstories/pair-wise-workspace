package demo_test

import (
	"testing"

	"mediacheck/internal/demo"
	"mediacheck/internal/media"
)

func allFaults() map[string]bool {
	return map[string]bool{
		"truncated": true, "stale-index": true, "rtp-wrap": true,
		"slow-reader": true, "crash-after-mux": true,
	}
}

func TestAllExpectedAcceptancePaths(t *testing.T) {
	err := demo.Run(demo.Config{Out: t.TempDir(), Faults: allFaults(), Resume: true})
	if err != nil {
		t.Fatalf("demo run: %v", err)
	}
}

func TestCrashWithoutResumeIsRecoverableExit75(t *testing.T) {
	err := demo.Run(demo.Config{
		Out: t.TempDir(), Resume: false,
		Faults: map[string]bool{"crash-after-mux": true},
	})
	if code := media.ExitCode(err); code != 75 {
		t.Fatalf("exit code=%d err=%v", code, err)
	}
}

func TestMissingOutputDirectoryIsInvalid(t *testing.T) {
	err := demo.Run(demo.Config{Out: ""})
	if media.ExitCode(err) != 2 {
		t.Fatalf("err=%v code=%d", err, media.ExitCode(err))
	}
}
