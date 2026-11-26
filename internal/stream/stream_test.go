package stream

import (
	"context"
	"errors"
	"testing"

	"mediaflow/internal/clock"
	"mediaflow/internal/remux"
	"mediaflow/internal/timeline"
)

func makeFrames(n int) []clock.Normalized {
	frames := make([]clock.Normalized, n)
	for i := range frames {
		frames[i] = clock.Normalized{
			Track: 1, Seq: uint32(i), StartUS: int64(i) * 40_000,
			Duration: 40_000, Payload: []byte{byte(i), byte(i >> 8)},
		}
	}
	return frames
}

func newRunner(t *testing.T, dir string) (*Runner, *Checkpointer, *remux.Writer) {
	t.Helper()
	w, err := remux.NewWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := NewCheckpointer(dir, "checkpoint.json")
	if err != nil {
		t.Fatal(err)
	}
	return NewRunner(w, cp, RunConfig{OutputName: "out.bin", MaxFramesPerSegment: 8}), cp, w
}

func TestFreshRunWithP95(t *testing.T) {
	dir := t.TempDir()
	r, _, _ := newRunner(t, dir)
	frames := makeFrames(40)
	stats, err := r.Run(context.Background(), frames, "inputsha")
	if err != nil {
		t.Fatal(err)
	}
	if stats.Segments != 5 || stats.FramesWritten != 40 {
		t.Fatalf("segment/frame counts wrong: %+v", stats)
	}
	if stats.ResumeUsed || stats.ResumedFrom != 0 {
		t.Fatal("fresh run must not report resume")
	}
	if stats.P95US < 0 || len(stats.OutputSHA) != 64 {
		t.Fatalf("metrics wrong: %+v", stats)
	}
}

func TestResumeFromCheckpoint(t *testing.T) {
	dir := t.TempDir()
	frames := makeFrames(40)
	inputSHA := "stable-input"

	// Pre-seed a valid checkpoint at the segment-2 complete boundary.
	_, cp1, _ := newRunner(t, dir)
	seed := Checkpoint{InputSHA: inputSHA, NextSeg: 2, OutputName: "out.bin"}
	actual := make([]string, 5)
	for i := range actual {
		seg := groupSegments(frames, 8)[i]
		b, _ := remux.EncodeFrames(seg.Frames)
		actual[i] = hashSegment(b)
	}
	seed.SegSHA = append(seed.SegSHA, actual[0], actual[1])
	if err := cp1.Save(seed); err != nil {
		t.Fatal(err)
	}

	// A fresh runner must honor the checkpoint and finish the remaining work.
	r2, cp2, _ := newRunner(t, dir)
	stats, err := r2.Run(context.Background(), frames, inputSHA)
	if err != nil {
		t.Fatal(err)
	}
	if !stats.ResumeUsed || stats.ResumedFrom != 2 {
		t.Fatalf("did not resume from segment 2: %+v", stats)
	}
	if stats.FramesWritten != 40 {
		t.Fatalf("resumed run must write all frames, got %d", stats.FramesWritten)
	}
	// Checkpoint removed after clean completion.
	if _, err := cp2.Load(); err != nil {
		t.Fatalf("load after completion: %v", err)
	}
}

func TestResumeRejectsChangedInput(t *testing.T) {
	cp := Checkpoint{InputSHA: "old", NextSeg: 1, SegSHA: []string{"abc"}}
	actual := []string{"abc", "def"}
	if err := VerifyResume(cp, "different", actual); !errors.Is(err, timeline.ErrCorruptIndex) {
		t.Fatalf("changed input must invalidate resume: %v", err)
	}
}

func TestResumeRejectsSegmentChecksumMismatch(t *testing.T) {
	cp := Checkpoint{InputSHA: "same", NextSeg: 2, SegSHA: []string{"aaa", "bbb"}}
	// First segment checksum differs -> must not trust the partial output.
	actual := []string{"aaa", "xxx"}
	if err := VerifyResume(cp, "same", actual); !errors.Is(err, timeline.ErrCorruptIndex) {
		t.Fatalf("segment checksum drift must invalidate resume: %v", err)
	}
}

func TestResumeAcceptsMatchingState(t *testing.T) {
	cp := Checkpoint{InputSHA: "same", NextSeg: 2, SegSHA: []string{"aaa", "bbb"}}
	actual := []string{"aaa", "bbb", "ccc"}
	if err := VerifyResume(cp, "same", actual); err != nil {
		t.Fatalf("matching state must resume: %v", err)
	}
}

func TestDeterministicOutputAcrossRuns(t *testing.T) {
	frames := makeFrames(40)
	dir1, dir2 := t.TempDir(), t.TempDir()
	r1, _, _ := newRunner(t, dir1)
	r2, _, _ := newRunner(t, dir2)
	s1, err := r1.Run(context.Background(), frames, "in")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := r2.Run(context.Background(), frames, "in")
	if err != nil {
		t.Fatal(err)
	}
	if s1.OutputSHA != s2.OutputSHA {
		t.Fatal("same input produced different output hashes")
	}
}

// TestRealInterruptAndResume verifies a genuine cancellation followed by a
// resume from the nearest complete segment boundary.
func TestRealInterruptAndResume(t *testing.T) {
	dir := t.TempDir()
	frames := makeFrames(40) // 5 segments of 8 frames
	inputSHA := "interrupt-input"

	// Phase 1: run with an already-canceled context so the first segment boundary
	// triggers ErrCanceled after persisting a checkpoint at a complete boundary.
	r1, cp, _ := newRunner(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r1.Run(ctx, frames, inputSHA)
	if !errors.Is(err, timeline.ErrCanceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	saved, err := cp.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.NextSeg < 1 || saved.NextSeg > 5 {
		t.Fatalf("checkpoint must land on a complete boundary, got %d", saved.NextSeg)
	}
	if len(saved.SegSHA) != saved.NextSeg {
		t.Fatalf("checkpoint segment checksums %d != next %d", len(saved.SegSHA), saved.NextSeg)
	}

	// Phase 2: a fresh runner resumes from the persisted boundary and completes.
	r2, cp2, _ := newRunner(t, dir)
	stats, err := r2.Run(context.Background(), frames, inputSHA)
	if err != nil {
		t.Fatal(err)
	}
	if !stats.ResumeUsed || stats.ResumedFrom != saved.NextSeg {
		t.Fatalf("did not resume from boundary %d: %+v", saved.NextSeg, stats)
	}
	if stats.FramesWritten != 40 {
		t.Fatalf("resumed run must cover all frames, got %d", stats.FramesWritten)
	}

	// The resumed output must byte-match a clean uninterrupted run.
	fresh, _, _ := newRunner(t, t.TempDir())
	clean, err := fresh.Run(context.Background(), frames, inputSHA)
	if err != nil {
		t.Fatal(err)
	}
	if stats.OutputSHA != clean.OutputSHA {
		t.Fatal("interrupted/resumed output hash differs from clean run")
	}
	if _, err := cp2.Load(); err != nil {
		t.Fatalf("checkpoint should remain readable: %v", err)
	}
}
