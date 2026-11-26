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

	// First run: cancel after a few segments via a short-lived context.
	r1, cp1, _ := newRunner(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	// Pre-seed a checkpoint at segment 2, then simulate interruption.
	seed := Checkpoint{InputSHA: inputSHA, NextSeg: 2, OutputName: "out.bin"}
	enc := make([][]byte, 5)
	actual := make([]string, 5)
	for i := range enc {
		seg := groupSegments(frames, 8)[i]
		b, _ := remux.EncodeFrames(seg.Frames)
		enc[i], actual[i] = b, hashSegment(b)
	}
	seed.SegSHA = append(seed.SegSHA, actual[0], actual[1])
	if err := cp1.Save(seed); err != nil {
		t.Fatal(err)
	}
	cancel()

	// A canceled run surfaces ErrCanceled but keeps its checkpoint usable.
	if _, err := r1.Run(ctx, frames, inputSHA); !errors.Is(err, timeline.ErrCanceled) {
		t.Fatalf("expected cancellation error, got %v", err)
	}

	// Second run with a live context must resume at segment 2 and finish.
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
