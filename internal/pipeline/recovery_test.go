package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/core"
	"mediad/internal/fixture"
)

func cancelDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := fixture.Generate(fixture.CancelConfig(filepath.Join(dir, MediaDir))); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runCancelGate(t *testing.T, dir string) *Report {
	t.Helper()
	ctx := context.Background()
	cfg := Config{Workers: 1, Queue: 2}
	golden, err := DryRun(ctx, filepath.Join(dir, MediaDir), MediaDir, dir, cfg)
	if err != nil {
		t.Fatalf("dryrun: %v", err)
	}
	rep, err := Execute(ctx, ExecuteParams{
		MediaRoot:      filepath.Join(dir, MediaDir),
		CheckpointPath: filepath.Join(dir, CheckpointName),
		Case:           "cancel", Config: cfg, CancelAfterCommits: 3,
		ExpectedHash: golden.ResultHash,
	})
	if err != nil {
		t.Fatalf("cancel execute: %v", err)
	}
	return rep
}

func TestCancelLeavesExactlyOnePendingAndResumeMatches(t *testing.T) {
	dir := cancelDir(t)
	rep := runCancelGate(t, dir)
	if rep.ExitCode != core.ExitCanceled || !rep.Canceled {
		t.Fatalf("want exit 3 canceled, got code=%d canceled=%v", rep.ExitCode, rep.Canceled)
	}
	if rep.Committed != 3 {
		t.Fatalf("committed=%d want 3", rep.Committed)
	}
	cp, err := LoadCheckpoint(filepath.Join(dir, CheckpointName))
	if err != nil {
		t.Fatal(err)
	}
	if len(cp.Committed) != 3 || cp.State != StateCanceled {
		t.Fatalf("checkpoint wrong: n=%d state=%s", len(cp.Committed), cp.State)
	}

	res, err := ResumeDir(context.Background(), dir)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.Resumed != 1 {
		t.Errorf("resumed=%d want 1", res.Resumed)
	}
	if res.DuplicateEmit != 0 {
		t.Errorf("duplicate_emit=%d want 0", res.DuplicateEmit)
	}
	if !res.ResultHashEqual {
		t.Errorf("result hash mismatch: got=%s expected=%s", res.ResultHash, res.ExpectedHash)
	}
	if res.ExitCode != 0 || res.Manifest == nil {
		t.Fatalf("resume exit=%d manifest=%v", res.ExitCode, res.Manifest != nil)
	}

	// A second resume is idempotent and emits nothing new.
	res2, err := ResumeDir(context.Background(), dir)
	if err != nil {
		t.Fatalf("second resume: %v", err)
	}
	if res2.Resumed != 0 || res2.DuplicateEmit != 0 || !res2.ResultHashEqual {
		t.Fatalf("idempotent resume wrong: %+v", res2)
	}
}

func TestResumeRejectsInputDigestChange(t *testing.T) {
	dir := cancelDir(t)
	runCancelGate(t, dir)

	media := filepath.Join(dir, MediaDir, "cam01", "high")
	entries, _ := os.ReadDir(media)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".m4s" {
			f := filepath.Join(media, e.Name())
			_ = os.WriteFile(f, append(mustRead(t, f), 0x00), 0o644)
			break
		}
	}
	_, err := ResumeDir(context.Background(), dir)
	if code := core.ExitCode(err); code != core.ExitFatal {
		t.Fatalf("tampered input: err=%v exit=%d want 1", err, code)
	}
	var ie *core.InputError
	if !errors.As(err, &ie) {
		t.Fatalf("want InputError, got %T", err)
	}
}

func TestResumeRejectsConfigDigestChange(t *testing.T) {
	dir := cancelDir(t)
	runCancelGate(t, dir)
	path := filepath.Join(dir, CheckpointName)
	cp, err := LoadCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	cp.Processing = Config{Workers: 99, Queue: 99}
	if err := cp.Save(core.NewManualClock(core.Epoch)); err != nil {
		t.Fatal(err)
	}
	_, err = ResumeDir(context.Background(), dir)
	var ce *core.ConfigError
	if !errors.As(err, &ce) || core.ExitCode(err) != 1 {
		t.Fatalf("want ConfigError exit1, got %v", err)
	}
}

func TestAlreadyCanceledContextCommitsNothing(t *testing.T) {
	dir := cancelDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := Execute(ctx, ExecuteParams{
		MediaRoot:      filepath.Join(dir, MediaDir),
		CheckpointPath: filepath.Join(dir, CheckpointName),
		Config:         Config{Workers: 2, Queue: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.ExitCode != 3 || rep.Committed != 0 {
		t.Fatalf("pre-canceled run: code=%d committed=%d want 3/0", rep.ExitCode, rep.Committed)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
