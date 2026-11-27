package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"relayaudit/internal/checkpoint"
	"relayaudit/internal/fault"
	"relayaudit/internal/fixture"
	"relayaudit/internal/verify"
)

func newStore(t *testing.T) *checkpoint.Store {
	t.Helper()
	s, err := checkpoint.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return s
}

func source(t *testing.T, spec fixture.Spec) []byte {
	t.Helper()
	seg, err := fixture.Generate(spec)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return seg.Bytes
}

func TestRunCompleteInputProducesVerifiedOutput(t *testing.T) {
	src := source(t, fixture.Spec{Seed: 73, Size: 1 << 18})
	store := newStore(t)
	out, err := Run(context.Background(), Config{Source: src, Seed: 73, Store: store})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.Interrupted || out.AlreadyComplete || out.Resumed {
		t.Fatalf("flags = %+v", out)
	}
	if len(out.Output) == 0 {
		t.Fatalf("output missing")
	}
	if len(out.OutputSHA256) != 64 {
		t.Fatalf("digest = %q", out.OutputSHA256)
	}
	if out.Kept+out.Dropped != int(out.Scan.Header.RecordCount) {
		t.Fatalf("kept %d + dropped %d != declared %d", out.Kept, out.Dropped, out.Scan.Header.RecordCount)
	}
	onDisk, err := store.ReadOutput()
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if verify.SHA256Hex(onDisk) != out.OutputSHA256 {
		t.Fatalf("persisted output digest differs")
	}
	st, ok, err := store.Load()
	if err != nil || !ok || !st.Done {
		t.Fatalf("checkpoint not finalised: ok=%v err=%v", ok, err)
	}
}

func TestRunRepeatIsIdempotent(t *testing.T) {
	src := source(t, fixture.Spec{Seed: 73, Size: 1 << 18})
	store := newStore(t)
	first, err := Run(context.Background(), Config{Source: src, Store: store})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := Run(context.Background(), Config{Source: src, Store: store, Resume: true})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !second.AlreadyComplete {
		t.Fatalf("repeat run re-processed a finished checkpoint")
	}
	if second.OutputSHA256 != first.OutputSHA256 {
		t.Fatalf("repeat digest %s != %s", second.OutputSHA256, first.OutputSHA256)
	}
	if second.Kept != first.Kept || second.Dropped != first.Dropped {
		t.Fatalf("repeat counts changed: %d/%d vs %d/%d",
			second.Kept, second.Dropped, first.Kept, first.Dropped)
	}
}

func TestRunInterruptThenResumeMatchesSinglePass(t *testing.T) {
	src := source(t, fixture.Spec{Seed: 73, Size: 1 << 18})

	straight, err := Run(context.Background(), Config{Source: src, Store: newStore(t)})
	if err != nil {
		t.Fatalf("straight: %v", err)
	}

	store := newStore(t)
	_, err = Run(context.Background(), Config{Source: src, Store: store, InterruptAfter: 60})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("expected interruption, got %v", err)
	}
	st, ok, err := store.Load()
	if err != nil || !ok {
		t.Fatalf("no checkpoint after interruption: ok=%v err=%v", ok, err)
	}
	if st.Done {
		t.Fatalf("interrupted run marked done")
	}
	if st.NextIndex != 60 {
		t.Fatalf("checkpoint cursor = %d, want 60", st.NextIndex)
	}

	resumed, err := Run(context.Background(), Config{Source: src, Store: store, Resume: true})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !resumed.Resumed {
		t.Fatalf("run did not report resuming")
	}
	if resumed.OutputSHA256 != straight.OutputSHA256 {
		t.Fatalf("resumed digest %s != straight %s", resumed.OutputSHA256, straight.OutputSHA256)
	}
	if resumed.Kept != straight.Kept || resumed.Dropped != straight.Dropped {
		t.Fatalf("resumed counts %d/%d != %d/%d",
			resumed.Kept, resumed.Dropped, straight.Kept, straight.Dropped)
	}
	if resumed.ResumeMillis > 2000 {
		t.Fatalf("resume took %d ms", resumed.ResumeMillis)
	}
}

func TestRunResumeIsIdempotent(t *testing.T) {
	src := source(t, fixture.Spec{Seed: 73, Size: 1 << 17})
	store := newStore(t)
	if _, err := Run(context.Background(), Config{Source: src, Store: store, InterruptAfter: 120}); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("interrupt: %v", err)
	}
	a, err := Run(context.Background(), Config{Source: src, Store: store, Resume: true})
	if err != nil {
		t.Fatalf("resume 1: %v", err)
	}
	b, err := Run(context.Background(), Config{Source: src, Store: store, Resume: true})
	if err != nil {
		t.Fatalf("resume 2: %v", err)
	}
	if a.OutputSHA256 != b.OutputSHA256 {
		t.Fatalf("second resume changed the digest")
	}
	if !b.AlreadyComplete {
		t.Fatalf("third run was not recognised as complete")
	}
}

func TestRunCanceledKeepsValidCheckpoint(t *testing.T) {
	src := source(t, fixture.Spec{Seed: 73, Size: 1 << 18})
	store := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := Run(ctx, Config{Source: src, Store: store, BatchRows: 10})
	if !fault.IsKind(err, fault.Canceled) {
		t.Fatalf("err = %v, want canceled", err)
	}
	if out.Output != nil {
		t.Fatalf("canceled run produced final output")
	}
	st, ok, err := store.Load()
	if err != nil {
		t.Fatalf("load after cancel: %v", err)
	}
	if ok && st.Done {
		t.Fatalf("canceled run marked done")
	}
	// A later resume must still produce a valid result.
	got, err := Run(context.Background(), Config{Source: src, Store: store, Resume: true})
	if err != nil {
		t.Fatalf("resume after cancel: %v", err)
	}
	if len(got.OutputSHA256) != 64 {
		t.Fatalf("resume after cancel did not finish")
	}
}

func TestRunRejectsCheckpointFromAnotherSource(t *testing.T) {
	srcA := source(t, fixture.Spec{Seed: 1, Size: 1 << 17})
	srcB := source(t, fixture.Spec{Seed: 2, Size: 1 << 17})
	store := newStore(t)
	if _, err := Run(context.Background(), Config{Source: srcA, Store: store, InterruptAfter: 100}); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("interrupt: %v", err)
	}
	_, err := Run(context.Background(), Config{Source: srcB, Store: store, Resume: true})
	if !fault.IsKind(err, fault.Checkpoint) {
		t.Fatalf("foreign source accepted: %v", err)
	}
}

func TestRunRejectsEmptySource(t *testing.T) {
	store := newStore(t)
	if _, err := Run(context.Background(), Config{Source: nil, Store: store}); !fault.IsKind(err, fault.Truncated) {
		t.Fatalf("empty source: %v", err)
	}
}

func TestRunRequiresStore(t *testing.T) {
	if _, err := Run(context.Background(), Config{Source: []byte("x")}); err == nil {
		t.Fatalf("nil store accepted")
	}
}

func TestRunRejectsCorruptCheckpointArtefacts(t *testing.T) {
	src := source(t, fixture.Spec{Seed: 3, Size: 1 << 17})
	store := newStore(t)
	if _, err := Run(context.Background(), Config{Source: src, Store: store, InterruptAfter: 80}); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("interrupt: %v", err)
	}
	// Truncate the staged prefix so it no longer matches the checkpoint.
	path := store.StagingPath()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read staging: %v", err)
	}
	if err := os.WriteFile(path, body[:len(body)/2], 0o644); err != nil {
		t.Fatalf("write staging: %v", err)
	}
	_, err = Run(context.Background(), Config{Source: src, Store: store, Resume: true})
	if !fault.IsKind(err, fault.Checkpoint) {
		t.Fatalf("damaged staging accepted: %v", err)
	}
	_ = filepath.Base
}

func TestRunResumeMillisOnlyReportedOnResume(t *testing.T) {
	src := source(t, fixture.Spec{Seed: 4, Size: 1 << 17})
	out, err := Run(context.Background(), Config{Source: src, Store: newStore(t)})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.ResumeMillis != 0 {
		t.Fatalf("fresh run reported resume time %d", out.ResumeMillis)
	}
}
