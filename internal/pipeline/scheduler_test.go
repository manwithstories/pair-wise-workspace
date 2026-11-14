package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"turncode/internal/config"
	"turncode/internal/fixture"
	"turncode/internal/report"
)

func makeTestFixture(t *testing.T) (config.Spec, string) {
	t.Helper()
	dir := t.TempDir()
	spec, err := fixture.Generate(fixture.Options{
		Width: 32, Height: 18, FPS: 10, Frames: 30, Scene: "mixed", Seed: 9, OutDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var loaded config.Spec
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	loaded.GOP, loaded.SegmentFrames = 10, 10
	if err := loaded.Validate(); err != nil {
		t.Fatal(err)
	}
	return loaded, filepath.Join(dir, spec.YUVFile)
}

func TestRunInterruptAndResume(t *testing.T) {
	spec, source := makeTestFixture(t)
	work := filepath.Join(t.TempDir(), "work")
	job := &Job{Spec: spec, SourcePath: source, WorkDir: work, FailAfterGOP: 1}
	out, err := RunJob(context.Background(), job)
	if !errors.Is(err, ErrInterrupted) || out.CompletedGOPs != 1 {
		t.Fatalf("interrupt: out=%+v err=%v", out, err)
	}
	resumed, err := LoadJob(work)
	if err != nil {
		t.Fatal(err)
	}
	out, err = RunJob(context.Background(), resumed)
	if err != nil || !out.Completed {
		t.Fatalf("resume: out=%+v err=%v", out, err)
	}
	if out.ResumedFromGOP != 1 || out.CompletedGOPs != 3 {
		t.Fatalf("resume accounting: %+v", out)
	}
	info, err := os.Stat(filepath.Join(work, "bitstream.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != out.Checkpoint.BitstreamOffset {
		t.Fatal("bitstream/checkpoint offset mismatch")
	}
	rep := report.Build(report.Input{Spec: spec, BitstreamBytes: info.Size(), Checkpoint: out.Checkpoint})
	if rep.GOPCount != 3 || rep.PSNRMeanDB <= 0 {
		t.Fatalf("bad report: %+v", rep)
	}
}

func TestSchedulerRunsJobsConcurrently(t *testing.T) {
	spec, source := makeTestFixture(t)
	jobs := []*Job{
		{Spec: spec, SourcePath: source, WorkDir: filepath.Join(t.TempDir(), "a")},
		{Spec: spec, SourcePath: source, WorkDir: filepath.Join(t.TempDir(), "b")},
	}
	results := NewScheduler(2).Run(context.Background(), jobs)
	for _, result := range results {
		if result.Err != nil || !result.Outcome.Completed {
			t.Fatalf("scheduler result: %+v", result)
		}
	}
}
