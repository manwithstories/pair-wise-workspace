package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mediq/internal/fixture"
)

// TestResumeReproducesCleanRunStats interrupts a job that contains a
// duplicate, a reordering, a wrap and a corrupt frame, resumes it, and checks
// that every statistic and the published digest match an uninterrupted run.
func TestResumeReproducesCleanRunStats(t *testing.T) {
	dir := t.TempDir()
	if _, err := fixture.Generate(fixture.Config{
		Dir: dir, Frames: 1200, FrameBytes: 64, Duplicate: 300,
		OutOfOrder: 600, WrapAt: 900, Corrupt: 1050, Seed: 5,
	}); err != nil {
		t.Fatal(err)
	}
	base := Options{
		Input: filepath.Join(dir, "damaged.seq"), Output: filepath.Join(dir, "a.mp4"),
		CheckpointPath: filepath.Join(dir, "a.ckpt"), ChunkFrames: 32, QueueDepth: 4,
	}
	clean, err := New(base).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < 3; attempt++ {
		os.Remove(filepath.Join(dir, "b.mp4"))
		os.Remove(filepath.Join(dir, "b.ckpt"))
		os.Remove(filepath.Join(dir, "b.ckpt.chunks"))
		inter := base
		inter.Output = filepath.Join(dir, "b.mp4")
		inter.CheckpointPath = filepath.Join(dir, "b.ckpt")
		inter.SlowConsumer = time.Millisecond

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, e := New(inter).Run(ctx); done <- e }()
		time.Sleep(time.Duration(6+attempt*9) * time.Millisecond)
		cancel()
		if firstErr := <-done; firstErr == nil {
			continue // finished before the cancellation landed
		}

		res, err := New(inter).Run(context.Background())
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if res.FramesRead != clean.FramesRead || res.FramesIndexed != clean.FramesIndexed ||
			res.FramesQuarantined != clean.FramesQuarantined || res.FramesBad != clean.FramesBad {
			t.Fatalf("attempt %d: clean read=%d idx=%d q=%d bad=%d; resumed read=%d idx=%d q=%d bad=%d",
				attempt, clean.FramesRead, clean.FramesIndexed, clean.FramesQuarantined, clean.FramesBad,
				res.FramesRead, res.FramesIndexed, res.FramesQuarantined, res.FramesBad)
		}
		if res.OutputSHA256 != clean.OutputSHA256 {
			t.Fatalf("attempt %d: resumed digest differs from the clean run", attempt)
		}
	}
}
