package pipeline

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"mediaaudit/internal/index"
	"mediaaudit/internal/model"
)

func testIndex(t *testing.T, total int64) *index.File {
	t.Helper()
	cfg := index.Config{
		PackagePath: "pkg", ChunkFrames: 100, CheckpointEvery: 4,
		TileFrames: 32, InputFingerprint: "fp",
	}
	f, err := index.Create(filepath.Join(t.TempDir(), "i.pfi"), cfg, total)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// chunkFn counts calls per seq and marks chunk seq 2 as damaged.
func chunkFn(calls *int64, damageSeq int64) ChunkFunc {
	return func(seq int64, r model.FrameRange) (int, error) {
		atomic.AddInt64(calls, 1)
		if seq == damageSeq {
			return 1, nil
		}
		return 0, nil
	}
}

func TestRunCommitsAllChunks(t *testing.T) {
	total := int64(500) // 5 chunks of 100 frames
	idx := testIndex(t, total)
	rn, err := NewRunner(idx, Params{ChunkFrames: 100, CheckpointEvery: 4, Workers: 3})
	if err != nil {
		t.Fatal(err)
	}
	var calls int64
	if err := rn.Run(context.Background(), total, chunkFn(&calls, 2)); err != nil {
		t.Fatal(err)
	}
	if calls != 5 {
		t.Fatalf("chunk calls=%d want 5", calls)
	}
	if !idx.IsFinalized() {
		t.Fatal("run should finalize index")
	}
	if idx.DamageCount() != 1 {
		t.Fatalf("damage=%d want 1", idx.DamageCount())
	}
	// Validate contiguous ranges.
	for i, c := range idx.Payload.Chunks {
		if c.FirstFrame != int64(i)*100 {
			t.Fatalf("chunk %d first=%d", i, c.FirstFrame)
		}
	}
}

func TestInterruptAtCheckpoint(t *testing.T) {
	total := int64(1000) // 10 chunks
	idx := testIndex(t, total)
	rn, _ := NewRunner(idx, Params{
		ChunkFrames: 100, CheckpointEvery: 1,
		InterruptAfterCheckpoint: 3, Workers: 1,
	})
	var calls int64
	err := rn.Run(context.Background(), total, chunkFn(&calls, -1))
	if !IsInterrupted(err) {
		t.Fatalf("expected interruption, got %v", err)
	}
	if !errors.Is(err, model.ErrInterrupted) {
		t.Fatal("interruption must anchor sentinel")
	}
	// Exactly 3 checkpoints committed before the stop.
	if len(idx.Payload.Chunks) != 3 {
		t.Fatalf("committed chunks=%d want 3", len(idx.Payload.Chunks))
	}
}

func TestResumeOnlyProcessesUncommitted(t *testing.T) {
	total := int64(500) // 5 chunks
	idx := testIndex(t, total)
	rn, _ := NewRunner(idx, Params{
		ChunkFrames: 100, CheckpointEvery: 1, InterruptAfterCheckpoint: 2, Workers: 1,
	})
	var calls int64
	_ = rn.Run(context.Background(), total, chunkFn(&calls, -1))
	if calls != 2 {
		t.Fatalf("first run calls=%d want 2", calls)
	}
	// Resume: only the remaining 3 chunks should be evaluated.
	rn2, _ := NewRunner(idx, Params{ChunkFrames: 100, CheckpointEvery: 1, Workers: 1})
	calls = 0
	if err := rn2.Run(context.Background(), total, chunkFn(&calls, -1)); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("resume processed %d chunks, want only 3 uncommitted", calls)
	}
	if !idx.IsFinalized() {
		t.Fatal("resume should finalize")
	}
}

func TestRecomputeDetectsMismatch(t *testing.T) {
	total := int64(300) // 3 chunks
	idx := testIndex(t, total)
	rn, _ := NewRunner(idx, Params{ChunkFrames: 100, CheckpointEvery: 1, Workers: 1})
	var calls int64
	if err := rn.Run(context.Background(), total, chunkFn(&calls, 1)); err != nil {
		t.Fatal(err)
	}
	// Same function -> verify passes.
	var vcalls int64
	if err := Recompute(idx, total, 100, chunkFn(&vcalls, 1)); err != nil {
		t.Fatalf("recompute with same fn should pass: %v", err)
	}
	// Different damage location -> verify must fail.
	var dcalls int64
	err := Recompute(idx, total, 100, chunkFn(&dcalls, 2))
	if !errors.Is(err, model.ErrIndexUnrecoverable) {
		t.Fatalf("expected index fault on mismatch, got %v", err)
	}
}

func TestRejectsOversizedChunk(t *testing.T) {
	idx := testIndex(t, 10)
	if _, err := NewRunner(idx, Params{ChunkFrames: MaxChunkFrames + 1, CheckpointEvery: 1}); err == nil {
		t.Fatal("expected rejection of oversized chunk")
	}
}
