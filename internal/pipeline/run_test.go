package pipeline_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mediad/internal/fixture"
	"mediad/internal/model"
	"mediad/internal/pipeline"
	"mediad/internal/trackmap"
)

func writeMedia(t *testing.T, dir, name string, seed int64) string {
	t.Helper()
	media, err := fixture.GenerateMedia(seed, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, media.Data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func allRequests() []trackmap.Request {
	return []trackmap.Request{
		{SourceID: 1, OutputID: 1},
		{SourceID: 2, OutputID: 2},
		{SourceID: 3, OutputID: 3},
		{SourceID: 4, OutputID: 4},
	}
}

func TestInterruptResumeMatchesFreshOutput(t *testing.T) {
	dir := t.TempDir()
	input := writeMedia(t, dir, "input.mp4", 17)
	req := allRequests()

	fresh, err := pipeline.Run(pipeline.Options{
		InputPath: input, OutputPath: filepath.Join(dir, "fresh.mp4"),
		CheckpointPath: filepath.Join(dir, "fresh.ckpt"), CachePath: filepath.Join(dir, "index.json"),
		Requests: req,
	})
	if err != nil {
		t.Fatalf("fresh run: %v", err)
	}
	if fresh.OutputSHA256 == "" || len(fresh.OutputSHA256) != 64 {
		t.Fatalf("bad fresh sha %q", fresh.OutputSHA256)
	}
	if fresh.Verification == nil || fresh.Verification.Samples != 96 {
		t.Fatalf("verification=%+v", fresh.Verification)
	}

	resumeOut := filepath.Join(dir, "resume.mp4")
	ckptPath := filepath.Join(dir, "resume.ckpt")
	cachePath := filepath.Join(dir, "resume-index.json")
	first, err := pipeline.Run(pipeline.Options{
		InputPath: input, OutputPath: resumeOut, CheckpointPath: ckptPath, CachePath: cachePath,
		Requests: req, InterruptAfter: 2,
	})
	if !errors.Is(err, pipeline.ErrInterrupted) {
		t.Fatalf("expected interrupt, got %v", err)
	}
	if first == nil || first.CacheState != "rebuilt" {
		t.Fatalf("first cache state=%+v", first)
	}
	if first.Checkpoint.CommittedSamples != 2 {
		t.Fatalf("committed=%d want 2", first.Checkpoint.CommittedSamples)
	}
	// Simulate a stale writer tail after the durable checkpoint boundary.
	f, err := os.OpenFile(resumeOut, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("uncommitted-tail")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	second, err := pipeline.Run(pipeline.Options{
		InputPath: input, OutputPath: resumeOut, CheckpointPath: ckptPath, CachePath: cachePath,
		Requests: req,
	})
	resumeMS := time.Since(start).Milliseconds()
	if err != nil {
		t.Fatalf("resume run: %v", err)
	}
	if second.CacheState != "hit" {
		t.Fatalf("cache state=%q want hit", second.CacheState)
	}
	if second.OutputSHA256 != fresh.OutputSHA256 {
		t.Fatalf("resume sha=%s fresh=%s", second.OutputSHA256, fresh.OutputSHA256)
	}
	if resumeMS >= 5000 {
		t.Fatalf("resume took %dms, want <5000", resumeMS)
	}

	again, err := pipeline.Run(pipeline.Options{
		InputPath: input, OutputPath: resumeOut, CheckpointPath: ckptPath, CachePath: cachePath,
		Requests: req,
	})
	if err != nil {
		t.Fatalf("repeat completed run: %v", err)
	}
	if again.OutputSHA256 != fresh.OutputSHA256 {
		t.Fatal("repeat execution changed output checksum")
	}
}

func TestIndexCacheInvalidatedByVersionAndInput(t *testing.T) {
	dir := t.TempDir()
	input := writeMedia(t, dir, "input.mp4", 17)
	other := writeMedia(t, dir, "other.mp4", 18)
	cache := filepath.Join(dir, "cache.json")
	first, err := pipeline.LoadOrBuildIndex(input, cache)
	if err != nil || first.State != "rebuilt" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	hit, err := pipeline.LoadOrBuildIndex(input, cache)
	if err != nil || hit.State != "hit" {
		t.Fatalf("hit=%+v err=%v", hit, err)
	}
	if err := pipeline.InvalidateIndexForTest(cache); err != nil {
		t.Fatal(err)
	}
	stale, err := pipeline.LoadOrBuildIndex(input, cache)
	if err != nil || stale.State != "invalidated" {
		t.Fatalf("stale=%+v err=%v", stale, err)
	}
	moved, err := pipeline.LoadOrBuildIndex(other, cache)
	if err != nil || moved.State != "invalidated" {
		t.Fatalf("input change state=%+v err=%v", moved, err)
	}
	if err := os.WriteFile(cache, []byte(`{"version":1,"index":`), 0o644); err != nil {
		t.Fatal(err)
	}
	truncated, err := pipeline.LoadOrBuildIndex(input, cache)
	if err != nil || truncated.State != "invalidated" || truncated.Index == nil {
		t.Fatalf("truncated cache state=%+v err=%v", truncated, err)
	}
}

func TestCorruptCheckpointFailsIntegrityContract(t *testing.T) {
	dir := t.TempDir()
	input := writeMedia(t, dir, "input.mp4", 17)
	out := filepath.Join(dir, "out.mp4")
	ckpt := filepath.Join(dir, "bad.ckpt")
	if err := os.WriteFile(ckpt, []byte("{this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := pipeline.Run(pipeline.Options{
		InputPath: input, OutputPath: out, CheckpointPath: ckpt,
		Requests: allRequests(),
	})
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.CodeCheckpointCorrupt {
		t.Fatalf("expected checkpoint_corrupt, got %v", err)
	}
	if model.ExitCode(err) != 4 {
		t.Fatalf("exit=%d want 4", model.ExitCode(err))
	}
}
