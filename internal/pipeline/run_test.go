package pipeline

import (
	"path/filepath"
	"testing"

	"bcast/internal/fixture"
	"bcast/internal/media"
	"bcast/internal/txn"
)

func TestTruncatedPipelineRollsBackLocatedFragment(t *testing.T) {
	out := t.TempDir()
	source := filepath.Join(out, "input")
	if _, err := fixture.Generate("truncated", source); err != nil {
		t.Fatal(err)
	}
	rep, err := Run(Options{Out: out, SourceDir: source})
	if media.ExitCodeOf(err) != media.ExitCorrupt {
		t.Fatalf("err=%v code=%d want 2", err, media.ExitCodeOf(err))
	}
	if rep == nil || rep.Status != "isolated" || rep.Rollback != 1 {
		t.Fatalf("report=%+v want isolated rollback=1", rep)
	}
	if rep.FirstErrorByte <= 0 {
		t.Fatalf("first_error_byte=%d want positive", rep.FirstErrorByte)
	}
	gens, err := txn.StagingGenerations(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(gens) != 0 {
		t.Fatalf("staging generations=%v want rolled back", gens)
	}
}

func TestFailAfterIndexResumeAndIdempotentSHA(t *testing.T) {
	out := t.TempDir()
	source := filepath.Join(out, "input")
	if _, err := fixture.Generate("drift-wrap-vfr", source); err != nil {
		t.Fatal(err)
	}
	_, err := Run(Options{Out: out, SourceDir: source, FailAfter: "index"})
	if media.ExitCodeOf(err) != media.ExitInterrupted {
		t.Fatalf("err=%v code=%d want 75", err, media.ExitCodeOf(err))
	}
	gens, err := txn.StagingGenerations(out)
	if err != nil || len(gens) != 1 {
		t.Fatalf("gens=%v err=%v want one interrupted generation", gens, err)
	}

	rep, err := Resume(out)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if rep.Recovered != 1 || rep.Committed != 1 {
		t.Fatalf("recovered=%d committed=%d want 1/1", rep.Recovered, rep.Committed)
	}
	if rep.SeekOK != 24 || rep.SeekTotal != 24 {
		t.Fatalf("seek=%d/%d want 24/24", rep.SeekOK, rep.SeekTotal)
	}
	if rep.ManifestSkewMS > 2 {
		t.Fatalf("skew=%.3f > 2ms", rep.ManifestSkewMS)
	}
	if rep.SHA256 == "" {
		t.Fatal("empty commit SHA")
	}

	again, err := Resume(out)
	if err != nil {
		t.Fatalf("second resume: %v", err)
	}
	if again.SHA256 != rep.SHA256 {
		t.Fatalf("SHA changed: %s vs %s", again.SHA256, rep.SHA256)
	}
	if again.SampleCount != rep.SampleCount || again.SeekOK != rep.SeekOK || again.ActualKBPS != rep.ActualKBPS {
		t.Fatalf("metrics changed: first=%+v second=%+v", rep, again)
	}
	if again.Committed != 1 {
		t.Fatalf("second committed=%d want 1", again.Committed)
	}
}

func TestMissingResumeReturnsIOExitCode(t *testing.T) {
	_, err := Resume(t.TempDir())
	if media.ExitCodeOf(err) != media.ExitIO {
		t.Fatalf("err=%v code=%d want 3", err, media.ExitCodeOf(err))
	}
}
