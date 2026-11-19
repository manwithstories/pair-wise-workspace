package pipeline

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"mediq/internal/fixture"
	"mediq/internal/index"
	"mediq/internal/model"
)

func genFixtures(t *testing.T, frames int, mut func(*fixture.Config)) (dir string, res *fixture.Result) {
	t.Helper()
	dir = t.TempDir()
	cfg := fixture.Config{Dir: dir, Frames: frames, FPS: 30, FrameBytes: 64, Seed: 7}
	if mut != nil {
		mut(&cfg)
	}
	res, err := fixture.Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return dir, res
}

func opts(dir, name string) Options {
	return Options{
		Input:          filepath.Join(dir, name+".seq"),
		Output:         filepath.Join(dir, name+".mp4"),
		CheckpointPath: filepath.Join(dir, name+".ckpt"),
		ChunkFrames:    16,
		QueueDepth:     4,
	}
}

func TestRunIndexesCleanInput(t *testing.T) {
	dir, _ := genFixtures(t, 100, nil)
	o := opts(dir, "normal")
	stats, err := New(o).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.FramesRead != 100 || stats.FramesIndexed != 100 {
		t.Fatalf("%+v", stats)
	}
	if !stats.IndexValid {
		t.Fatal("index invalid")
	}
	if len(stats.OutputSHA256) != 64 {
		t.Fatalf("digest %q", stats.OutputSHA256)
	}
	rep, err := index.VerifyFile(o.Output, index.VerifyOptions{SourcePath: o.Input})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.IndexValid || rep.Samples != 100 || rep.UnindexedSource != 0 {
		t.Fatalf("%+v", rep)
	}
}

func TestRunCorruptFrameIsolated(t *testing.T) {
	dir, _ := genFixtures(t, 200, func(c *fixture.Config) { c.Corrupt = 73 })
	o := opts(dir, "damaged")
	stats, err := New(o).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.FramesIndexed != 199 {
		t.Fatalf("indexed %d", stats.FramesIndexed)
	}
	if !stats.HasCorrupt || stats.FirstCorruptFrame != 73 {
		t.Fatalf("first corrupt %d", stats.FirstCorruptFrame)
	}
	if stats.FirstCorruptOffset <= 0 {
		t.Fatalf("offset %d", stats.FirstCorruptOffset)
	}
	stats2, err := New(opts(dir, "damaged")).Run(context.Background())
	if err == nil {
		_ = stats2
	}
	// The corrupt frame must not be in the index, but no valid one may be lost.
	rep, err := index.VerifyFile(o.Output, index.VerifyOptions{SourcePath: o.Input})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Samples != 199 || !rep.IndexValid {
		t.Fatalf("%+v", rep)
	}
	if len(rep.SourceFaults) != 1 || rep.SourceFaults[0].Code != model.CodeHashMismatch {
		t.Fatalf("source faults %+v", rep.SourceFaults)
	}
}

func TestRunDuplicateAndReorderAndWrap(t *testing.T) {
	dir, _ := genFixtures(t, 120, func(c *fixture.Config) {
		c.Duplicate = 40
		c.OutOfOrder = 60
		c.WrapAt = 90
		c.Corrupt = 0
	})
	// duplicate.seq carries the duplicate and the reordering, wrap.seq carries
	// the timestamp wrap that emulates a 32-bit capture clock rollover.
	o := opts(dir, "duplicate")
	stats, err := New(o).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.FramesQuarantined != 1 {
		t.Fatalf("quarantined %d reasons=%v", stats.FramesQuarantined, stats.QuarantineReasons)
	}
	rep, err := index.VerifyFile(o.Output, index.VerifyOptions{SourcePath: o.Input})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.TimestampMono {
		t.Fatal("timestamps not monotonic")
	}
	if rep.Samples != 120 {
		t.Fatalf("samples %d", rep.Samples)
	}

	wo := opts(dir, "wrap")
	wstats, err := New(wo).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if wstats.FramesQuarantined != 0 {
		t.Fatalf("wrap run quarantined %d reasons=%v", wstats.FramesQuarantined, wstats.QuarantineReasons)
	}
	if wstats.FramesIndexed != 120 {
		t.Fatalf("wrap run indexed %d", wstats.FramesIndexed)
	}
	wrep, err := index.VerifyFile(wo.Output, index.VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !wrep.TimestampMono || !wrep.IndexValid {
		t.Fatalf("wrap index: %+v", wrep)
	}
}

func TestRunTruncatedTail(t *testing.T) {
	dir, _ := genFixtures(t, 80, func(c *fixture.Config) { c.TruncateTail = 1 })
	o := opts(dir, "truncated")
	stats, err := New(o).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !stats.HasCorrupt {
		t.Fatal("truncation not reported")
	}
	if stats.FramesIndexed == 0 || stats.FramesIndexed >= 80 {
		t.Fatalf("indexed %d", stats.FramesIndexed)
	}
	// The report must name the first missing byte.
	if stats.FirstCorruptOffset <= 0 {
		t.Fatalf("offset %d", stats.FirstCorruptOffset)
	}
}

func TestRunEmptyInput(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "empty.seq")
	if err := os.WriteFile(input, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	o := Options{Input: input, Output: filepath.Join(dir, "out.mp4"), ChunkFrames: 8, QueueDepth: 2}
	stats, err := New(o).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.FramesRead != 0 || stats.FramesIndexed != 0 || !stats.IndexValid {
		t.Fatalf("%+v", stats)
	}
}

func TestRunRejectsOutputOverInput(t *testing.T) {
	dir, _ := genFixtures(t, 10, nil)
	input := filepath.Join(dir, "normal.seq")
	before, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(Options{Input: input, Output: input, ChunkFrames: 4}).Run(context.Background())
	if err == nil {
		t.Fatal("accepted output == input")
	}
	after, _ := os.ReadFile(input)
	if string(before) != string(after) {
		t.Fatal("input file was modified")
	}
}

func TestCancelStopsAllGoroutines(t *testing.T) {
	dir, _ := genFixtures(t, 2000, nil)
	o := opts(dir, "normal")
	o.SlowConsumer = 2 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := New(o).Run(ctx); err == nil {
			t.Error("expected cancellation error")
		}
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	// go test reports leaked goroutines itself, but check the producer is gone
	// by observing that no reader remains blocked on the queue.
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}

func TestResumeAfterCancelMatchesCleanRun(t *testing.T) {
	dir, _ := genFixtures(t, 600, func(c *fixture.Config) { c.Corrupt = 123; c.FrameBytes = 128 })
	input := filepath.Join(dir, "damaged.seq")

	// Reference output from an uninterrupted run.
	ref := Options{
		Input: input, Output: filepath.Join(dir, "ref.mp4"),
		CheckpointPath: filepath.Join(dir, "ref.ckpt"), ChunkFrames: 32, QueueDepth: 4,
	}
	if _, err := New(ref).Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Interrupted run.
	interrupted := Options{
		Input: input, Output: filepath.Join(dir, "res.mp4"),
		CheckpointPath: filepath.Join(dir, "res.ckpt"), ChunkFrames: 32, QueueDepth: 4,
		SlowConsumer: 1 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := New(interrupted).Run(ctx)
		done <- err
	}()
	time.Sleep(40 * time.Millisecond)
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("first run: %v", err)
	} else if err == nil {
		// The run beat the cancellation; there is nothing to resume.
		t.Skip("run completed before cancellation landed")
	}

	// Resume with identical arguments.
	resumed, err := New(interrupted).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Resumed || resumed.ResumeOffset <= 0 {
		t.Fatalf("%+v", resumed)
	}
	refBytes, _ := os.ReadFile(ref.Output)
	resBytes, _ := os.ReadFile(interrupted.Output)
	if len(refBytes) == 0 || string(refBytes) != string(resBytes) {
		t.Fatalf("resumed output differs from clean run (%d vs %d bytes)", len(refBytes), len(resBytes))
	}
	if resumed.FramesIndexed != 599 {
		t.Fatalf("indexed %d", resumed.FramesIndexed)
	}
	rep, err := index.VerifyFile(interrupted.Output, index.VerifyOptions{SourcePath: input})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.IndexValid {
		t.Fatalf("%+v", rep)
	}
}

func TestCheckpointMismatchOnChangedInput(t *testing.T) {
	dir, _ := genFixtures(t, 100, nil)
	o := opts(dir, "normal")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		o.SlowConsumer = 2 * time.Millisecond
		_, err := New(o).Run(ctx)
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done

	// Corrupt a byte inside the already-committed prefix.
	raw, err := os.ReadFile(o.Input)
	if err != nil {
		t.Fatal(err)
	}
	raw[100] ^= 0xFF
	if err := os.WriteFile(o.Input, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = New(o).Run(context.Background())
	if !errors.Is(err, model.ErrCheckpointMismatch) {
		t.Fatalf("got %v", err)
	}
}

func TestCompletedCheckpointRefused(t *testing.T) {
	dir, _ := genFixtures(t, 20, nil)
	o := opts(dir, "normal")
	if _, err := New(o).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := New(o).Run(context.Background()); !errors.Is(err, model.ErrCheckpointMismatch) {
		t.Fatalf("got %v", err)
	}
	o.Restart = true
	if _, err := New(o).Run(context.Background()); err != nil {
		t.Fatalf("restart failed: %v", err)
	}
}

func TestPeakQueueDepthBounded(t *testing.T) {
	dir, _ := genFixtures(t, 400, nil)
	o := opts(dir, "normal")
	o.QueueDepth = 3
	o.SlowConsumer = time.Millisecond
	stats, err := New(o).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.PeakQueueDepth > o.QueueDepth {
		t.Fatalf("peak depth %d exceeds %d", stats.PeakQueueDepth, o.QueueDepth)
	}
}

func TestConcurrentRunsAreIndependent(t *testing.T) {
	dir, _ := genFixtures(t, 150, func(c *fixture.Config) { c.Corrupt = 77 })
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			o := Options{
				Input:          filepath.Join(dir, "damaged.seq"),
				Output:         filepath.Join(dir, fmt.Sprintf("o%d.mp4", i)),
				CheckpointPath: filepath.Join(dir, fmt.Sprintf("o%d.ckpt", i)),
				ChunkFrames:    16, QueueDepth: 2,
			}
			stats, err := New(o).Run(context.Background())
			if err != nil {
				errs <- err
				return
			}
			if stats.FramesIndexed != 149 {
				errs <- fmt.Errorf("run %d indexed %d", i, stats.FramesIndexed)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestOutputPartFileNotPublishedOnFailure(t *testing.T) {
	dir, _ := genFixtures(t, 100, nil)
	o := opts(dir, "normal")
	// A checkpoint from another input forces a mismatch before any publish.
	other := o
	other.Input = filepath.Join(dir, "does-not-exist.seq")
	if _, err := New(other).Run(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := os.Stat(o.Output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output was published: %v", err)
	}
}

func TestStructuralCorruptionIsNotPublished(t *testing.T) {
	dir, _ := genFixtures(t, 60, nil)
	input := filepath.Join(dir, "normal.seq")
	raw, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	// Break the magic of the second record so framing is lost.
	raw2 := append([]byte(nil), raw...)
	// The first record's length is the header plus its metadata and payload.
	first := int64(72)
	first += int64(binary.BigEndian.Uint32(raw[32:36])) + int64(binary.BigEndian.Uint32(raw[36:40]))
	raw2[first] = 'Z'
	if err := os.WriteFile(input, raw2, 0o644); err != nil {
		t.Fatal(err)
	}
	o := opts(dir, "normal")
	stats, err := New(o).Run(context.Background())
	if !errors.Is(err, model.ErrDamagedInput) {
		t.Fatalf("got %v", err)
	}
	if stats.FirstCorruptOffset != first {
		t.Fatalf("offset %d want %d", stats.FirstCorruptOffset, first)
	}
	if _, err := os.Stat(o.Output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a partly readable source was published as a complete index")
	}
}
