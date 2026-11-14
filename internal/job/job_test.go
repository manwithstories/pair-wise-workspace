package job

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rcbench/internal/rate"
	"rcbench/internal/source"
)

func TestStateMachineLegal(t *testing.T) {
	m := NewMachine(StatePending)
	if err := m.Transition(StateRunning); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(StateInterrupted); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(StateRunning); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(StateCompleted); err != nil {
		t.Fatal(err)
	}
}

func TestStateMachineRejectsIllegal(t *testing.T) {
	m := NewMachine(StatePending)
	err := m.Transition(StateCompleted)
	if err == nil {
		t.Fatal("pending -> completed must be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, "pending") || !strings.Contains(msg, "running") {
		t.Fatalf("error should name current state and legal successors: %v", msg)
	}
	// completed is terminal
	m2 := NewMachine(StateCompleted)
	if err := m2.Transition(StateRunning); err == nil {
		t.Fatal("completed must be terminal")
	}
	if got := StateCompleted.Successors(); len(got) != 0 {
		t.Fatalf("completed should have no successors, got %v", got)
	}
}

func TestCheckpointRoundtrip(t *testing.T) {
	dir := t.TempDir()
	ck := &SegmentCheckpoint{Segment: 3, Frames: 30, QP: 34, Bits: 123456, PSNR: 38.5, Hash: "abc"}
	if err := WriteCheckpoint(dir, ck); err != nil {
		t.Fatal(err)
	}
	got, err := ReadCheckpoint(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	if *got != *ck {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", got, ck)
	}
	if _, err := ReadCheckpoint(dir, 4); err == nil {
		t.Fatal("missing checkpoint should error")
	}
}

func TestCheckpointCorrupt(t *testing.T) {
	dir := t.TempDir()
	ck := &SegmentCheckpoint{Segment: 1, Frames: 10, QP: 20, Bits: 100, PSNR: 40, Hash: "x"}
	if err := WriteCheckpoint(dir, ck); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "segments", "001.chk")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCheckpoint(dir, 1); err == nil {
		t.Fatal("corrupt checkpoint should error")
	}
}

// setupTestJob writes a small generated source and a job manifest.
func setupTestJob(t *testing.T, frames, segFrames int) (string, *Meta, *source.Reader) {
	t.Helper()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.yuv")
	cfg := source.Config{Seed: 11, Frames: frames, Width: 64, Height: 64}
	if _, err := source.GenerateFile(srcPath, cfg); err != nil {
		t.Fatal(err)
	}
	rdr, err := source.OpenReader(srcPath, 64, 64)
	if err != nil {
		t.Fatal(err)
	}
	jobDir := filepath.Join(dir, "job")
	meta := &Meta{
		Input: srcPath, Width: 64, Height: 64, FPS: 30, TargetBps: 400000,
		SegmentFrames: segFrames, TotalFrames: frames,
		Segments: (frames + segFrames - 1) / segFrames, State: string(StateRunning),
	}
	if err := WriteMeta(jobDir, meta); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rdr.Close() })
	return jobDir, meta, rdr
}

func TestSchedulerWorkerInvariance(t *testing.T) {
	run := func(workers int) []SegmentOutcome {
		jobDir, meta, rdr := setupTestJob(t, 24, 6)
		ctrl := rate.NewController(meta.TargetBps, meta.FPS, meta.SegmentFrames)
		out, err := Run(context.Background(), jobDir, meta, rdr, ctrl, Options{Workers: workers, MaxRetries: 1})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	a, b := run(1), run(4)
	if len(a) != len(b) {
		t.Fatalf("outcome length %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Bits != b[i].Bits || a[i].QP != b[i].QP || a[i].PSNR != b[i].PSNR {
			t.Fatalf("segment %d differs between workers: %+v vs %+v", i, a[i], b[i])
		}
	}
}

func TestSchedulerPoisonAndResume(t *testing.T) {
	jobDir, meta, rdr := setupTestJob(t, 24, 6)
	ctrl := rate.NewController(meta.TargetBps, meta.FPS, meta.SegmentFrames)
	_, err := Run(context.Background(), jobDir, meta, rdr, ctrl, Options{Workers: 2, MaxRetries: 1, FailAtSegment: 3})
	if err == nil || !strings.Contains(err.Error(), "poison") {
		t.Fatalf("expected poison error, got %v", err)
	}
	// segments 1-2 checkpointed, 3-4 missing
	if _, err := ReadCheckpoint(jobDir, 2); err != nil {
		t.Fatal("segment 2 should be checkpointed")
	}
	if _, err := ReadCheckpoint(jobDir, 3); err == nil {
		t.Fatal("segment 3 should not be checkpointed")
	}
	// resume without injection completes; finished segments are skipped
	ctrl2 := rate.NewController(meta.TargetBps, meta.FPS, meta.SegmentFrames)
	out, err := Run(context.Background(), jobDir, meta, rdr, ctrl2, Options{Workers: 2, MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 {
		t.Fatalf("expected 4 outcomes, got %d", len(out))
	}
	if !out[0].Skipped || !out[1].Skipped {
		t.Fatal("segments 1-2 should be skipped on resume")
	}
	if out[2].Skipped || out[3].Skipped {
		t.Fatal("segments 3-4 should be re-encoded")
	}
	for _, o := range out {
		if _, err := ReadCheckpoint(jobDir, o.Segment); err != nil {
			t.Fatalf("segment %d missing checkpoint after resume", o.Segment)
		}
	}
}

func TestSchedulerCancel(t *testing.T) {
	jobDir, meta, rdr := setupTestJob(t, 24, 6)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ctrl := rate.NewController(meta.TargetBps, meta.FPS, meta.SegmentFrames)
	_, err := Run(ctx, jobDir, meta, rdr, ctrl, Options{Workers: 1})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
}

func TestMetaRoundtrip(t *testing.T) {
	dir := t.TempDir()
	m := &Meta{Input: "a.yuv", InputSHA256: "ff", Width: 64, Height: 64, FPS: 30,
		TargetBps: 1000, SegmentFrames: 10, TotalFrames: 100, Segments: 10,
		State: string(StateFailed), Poisoned: []int{3, 5}}
	if err := WriteMeta(dir, m); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMeta(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.TargetBps != 1000 || got.State != "failed" || len(got.Poisoned) != 2 || got.Poisoned[1] != 5 {
		t.Fatalf("meta mismatch: %+v", got)
	}
}
