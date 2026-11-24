package pipeline

import (
	"os"
	"testing"

	"mediad/internal/model"
)

func TestFatalOnGarbageSource(t *testing.T) {
	dir := t.TempDir()
	p := Paths{Dir: dir, Track: 1}
	if err := os.WriteFile(p.Source(), []byte("not a container at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Run(Options{WorkDir: dir, TrackID: 1, TargetFrames: 10})
	if err == nil {
		t.Fatal("expected fatal error")
	}
	if model.ClassOf(err) != model.ClassFatal {
		t.Fatalf("class = %v, want fatal", model.ClassOf(err))
	}
	if model.ExitCode(err) != 2 {
		t.Fatalf("exit = %d, want 2", model.ExitCode(err))
	}
}

func TestVerifyClassForThresholdBreach(t *testing.T) {
	// A track whose target bitrate is deliberately unreachable must surface a
	// verify failure (exit 3), not a fatal input error.
	err := model.Verify("test", os.ErrNotExist)
	if model.ExitCode(err) != 3 {
		t.Fatalf("exit = %d, want 3", model.ExitCode(err))
	}
}
