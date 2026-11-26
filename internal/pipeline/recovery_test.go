package pipeline_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"mediascan/internal/fixture"
	"mediascan/internal/index"
	"mediascan/internal/pipeline"
	"mediascan/internal/provenance"
)

func setup(t *testing.T, fault fixture.Fault) (string, string, *index.Index) {
	t.Helper()
	root := t.TempDir()
	if err := fixture.Generate(root, fault); err != nil {
		t.Fatalf("generate: %v", err)
	}
	state := t.TempDir()
	idx, err := index.Build(root, index.Config{Hashes: true})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return root, state, idx
}

func runOpts(root, state string, stopAfter int) pipeline.Options {
	return pipeline.Options{Root: root, StateDir: state, Hashes: true, Concurrency: 4, StopAfter: stopAfter}
}

func TestInterruptAndResume(t *testing.T) {
	root, state, idx := setup(t, fixture.FaultTruncatedBox)

	out1, err := pipeline.Run(idx, runOpts(root, state, 3))
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if !out1.Interrupted {
		t.Fatal("run 1 should be interrupted by stop-after")
	}
	if out1.NewProcessed < 3 {
		t.Fatalf("run 1 processed %d, want >= 3", out1.NewProcessed)
	}
	// Checkpoint file must exist and hold only valid states.
	data, err := os.ReadFile(filepath.Join(state, pipeline.CheckpointFileName))
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	var cp pipeline.Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		t.Fatalf("checkpoint json: %v", err)
	}
	pending := 0
	for _, st := range cp.States {
		switch st {
		case pipeline.StatePending, pipeline.StateProcessing:
			pending++
		case pipeline.StateDone, pipeline.StateFailed:
		default:
			t.Fatalf("illegal state %q in checkpoint", st)
		}
	}
	if pending == 0 {
		t.Fatal("interrupted run must leave pending segments")
	}

	out2, err := pipeline.Run(idx, runOpts(root, state, 0))
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if out2.Interrupted {
		t.Fatal("run 2 should complete")
	}
	if out2.NewProcessed != pending {
		t.Fatalf("run 2 processed %d, want %d (the pending remainder)", out2.NewProcessed, pending)
	}
	if len(out2.Damage) != 1 {
		t.Fatalf("damage ranges = %d, want 1", len(out2.Damage))
	}

	// Third run: nothing to do, identical provenance hash.
	out3, err := pipeline.Run(idx, runOpts(root, state, 0))
	if err != nil {
		t.Fatalf("run 3: %v", err)
	}
	if out3.NewProcessed != 0 {
		t.Fatalf("run 3 processed %d, want 0", out3.NewProcessed)
	}
	rec2 := provenance.Build(idx, out2.Checkpoint, out2.Damage)
	rec3 := provenance.Build(idx, out3.Checkpoint, out3.Damage)
	if rec2.SHA256() != rec3.SHA256() {
		t.Fatalf("report hash unstable: %s != %s", rec2.SHA256(), rec3.SHA256())
	}
}

func TestDeterministicAcrossConcurrency(t *testing.T) {
	root, state, idx := setup(t, fixture.FaultTruncatedBox)
	outA, err := pipeline.Run(idx, runOpts(root, state, 0))
	if err != nil {
		t.Fatalf("run A: %v", err)
	}
	recA := provenance.Build(idx, outA.Checkpoint, outA.Damage)

	state2 := t.TempDir()
	opts := runOpts(root, state2, 0)
	opts.Concurrency = 1
	outB, err := pipeline.Run(idx, opts)
	if err != nil {
		t.Fatalf("run B: %v", err)
	}
	recB := provenance.Build(idx, outB.Checkpoint, outB.Damage)
	if recA.SHA256() != recB.SHA256() {
		t.Fatal("report hash must not depend on concurrency")
	}
}

func TestMissingSegmentIsolated(t *testing.T) {
	root, state, idx := setup(t, fixture.FaultMissingSegment)
	out, err := pipeline.Run(idx, runOpts(root, state, 0))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(out.Damage) != 1 {
		t.Fatalf("damage ranges = %d, want 1", len(out.Damage))
	}
	res := out.Checkpoint.Results["hls/seg5.m4s"]
	if res == nil || res.State != pipeline.StateFailed {
		t.Fatalf("missing segment state = %v, want failed", res)
	}
	// Other tracks must not be affected.
	done, hashed := 0, 0
	for id, r := range out.Checkpoint.Results {
		if r.State == pipeline.StateDone {
			done++
		}
		if r.SHA256 != "" {
			hashed++
		}
		_ = id
	}
	if done != fixture.TotalSegments-1 {
		t.Fatalf("done = %d, want %d", done, fixture.TotalSegments-1)
	}
	if hashed != fixture.TotalSegments-1 {
		t.Fatalf("hashed = %d, want %d", hashed, fixture.TotalSegments-1)
	}
}

func TestProcessingStateResetOnLoad(t *testing.T) {
	root, state, idx := setup(t, fixture.FaultNone)
	// Simulate a crash: checkpoint with one segment stuck "processing".
	cp := pipeline.NewCheckpoint(idx)
	cp.States[idx.Segments[0].ID] = pipeline.StateProcessing
	if err := cp.Save(state); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := pipeline.LoadCheckpoint(state, idx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st := loaded.States[idx.Segments[0].ID]; st != pipeline.StatePending {
		t.Fatalf("stuck processing state = %q, want pending", st)
	}
	_ = root
}

func TestStaleCheckpointDiscarded(t *testing.T) {
	root, state, idx := setup(t, fixture.FaultNone)
	if _, err := pipeline.Run(idx, runOpts(root, state, 0)); err != nil {
		t.Fatalf("run: %v", err)
	}
	// Modify the input: digests diverge, checkpoint must reset.
	seg := filepath.Join(root, "hls", "seg1.m4s")
	data, err := os.ReadFile(seg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(seg, append(data, 0), 0o644); err != nil {
		t.Fatal(err)
	}
	idx2, rebuilt, err := index.LoadOrBuild(state, root, index.Config{Hashes: true})
	if err != nil || !rebuilt {
		t.Fatalf("index after input change: rebuilt=%v err=%v", rebuilt, err)
	}
	cp, err := pipeline.LoadCheckpoint(state, idx2)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for id, st := range cp.States {
		if st != pipeline.StatePending {
			t.Fatalf("state %s = %q after invalidation, want pending", id, st)
		}
	}
}

func TestCleanRunAllTerminal(t *testing.T) {
	root, state, idx := setup(t, fixture.FaultNone)
	out, err := pipeline.Run(idx, runOpts(root, state, 0))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Interrupted || len(out.Damage) != 0 {
		t.Fatalf("clean run: interrupted=%v damage=%v", out.Interrupted, out.Damage)
	}
	kf, hashed := 0, 0
	for _, r := range out.Checkpoint.Results {
		if len(r.Keyframes) > 0 {
			kf++
		}
		if r.SHA256 != "" {
			hashed++
		}
	}
	if kf != fixture.TotalSegments || hashed != fixture.TotalSegments {
		t.Fatalf("kf=%d hashed=%d, want %d each", kf, hashed, fixture.TotalSegments)
	}
}
