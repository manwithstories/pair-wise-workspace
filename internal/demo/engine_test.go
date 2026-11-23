package demo

import (
	"context"
	"path/filepath"
	"testing"

	"mediascan/internal/checkpoint"
	"mediascan/internal/fault"
	"mediascan/internal/pipeline"
)

func newTestSpec(frames int) Spec {
	return Spec{
		Seed:          42,
		Tracks:        4,
		Frames:        frames,
		CorruptAt:     int64(frames / 2),
		Timescale:     30_000,
		KeyEvery:      25,
		KeyGapLimit:   250_000,
		DriftLimit:    50_000,
		SegmentFrames: 100,
	}
}

func poolCfg() pipeline.Limits {
	return pipeline.Limits{MaxQueueLen: 16, MaxConcurrent: 4}
}

func TestFullRunDigestAndLocalization(t *testing.T) {
	spec := newTestSpec(1200)
	media, err := Build(spec)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(media)
	if err != nil {
		t.Fatal(err)
	}
	cp := filepath.Join(t.TempDir(), "cp.json")
	res, err := eng.Run(context.Background(), RunConfig{
		Media: media, Checkpoint: cp, Pool: poolCfg(),
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Canceled {
		t.Fatal("full run must not be canceled")
	}
	if len(res.Summary.SHA256) != 64 {
		t.Fatalf("bad digest %q", res.Summary.SHA256)
	}
	// Truncation fault must localize exactly at the corrupt frame/offset.
	tf, ok := fault.FirstByKind(res.Faults, fault.KindTruncatedBox)
	if !ok {
		t.Fatal("truncation fault missing")
	}
	if tf.FrameIndex != spec.CorruptAt {
		t.Fatalf("localization frame=%d want %d", tf.FrameIndex, spec.CorruptAt)
	}
	wantOff := FrameOffset(media, 0, int(spec.CorruptAt))
	if tf.OffsetBytes != wantOff {
		t.Fatalf("offset=%d want %d", tf.OffsetBytes, wantOff)
	}
	// At least two fault families must flow through the whole chain.
	kinds := map[fault.Kind]bool{}
	for _, f := range res.Faults {
		kinds[f.Kind] = true
	}
	if len(kinds) < 3 {
		t.Fatalf("expected >=3 fault families, got %d", len(kinds))
	}
}

func TestCancelThenResumeDigestIdentical(t *testing.T) {
	spec := newTestSpec(1200)
	media, err := Build(spec)
	if err != nil {
		t.Fatal(err)
	}
	cp := filepath.Join(t.TempDir(), "cp.json")

	// Run 1: canceled after 500 frames.
	eng1, _ := NewEngine(media)
	res1, err := eng1.Run(context.Background(), RunConfig{
		Media: media, Checkpoint: cp, CancelAfter: 500, Pool: poolCfg(),
	})
	if err != nil {
		t.Fatalf("cancel run: %v", err)
	}
	if !res1.Canceled {
		t.Fatal("expected cancellation")
	}
	// Exactly 500 frames should be persisted in 100-frame segments.
	store, err := checkpoint.Load(cp, media.Digest, checkpoint.DigestParams(spec.CanonicalParams()))
	if err != nil {
		t.Fatalf("load checkpoint: %v", err)
	}
	if store.CompletedUpTo() != 500 {
		t.Fatalf("checkpoint upTo=%d want 500", store.CompletedUpTo())
	}

	// Run 2: resume must recompute only the remaining 700 frames.
	eng2, _ := NewEngine(media)
	res2, err := eng2.Run(context.Background(), RunConfig{
		Media: media, Checkpoint: cp, Resume: true, Pool: poolCfg(),
	})
	if err != nil {
		t.Fatalf("resume run: %v", err)
	}
	if res2.Canceled {
		t.Fatal("resume should complete")
	}
	if res2.Summary.RecomputedFrames != 700 {
		t.Fatalf("recomputed=%d want 700", res2.Summary.RecomputedFrames)
	}
	// The canceled partial run has fewer faults by construction; its digest
	// therefore differs, which is expected. The acceptance contract is that
	// two *complete* runs — one straight through, one via cancel+resume —
	// produce the identical SHA-256. That is checked below (res2 vs res3).
	if res1.Summary.SHA256 == res2.Summary.SHA256 {
		t.Fatal("partial canceled run must not hash equal to the complete run")
	}
	// The resumed full fault ordering must equal a fresh full run.
	fresh, _ := NewEngine(media)
	cp2 := filepath.Join(t.TempDir(), "cp2.json")
	res3, err := fresh.Run(context.Background(), RunConfig{
		Media: media, Checkpoint: cp2, Pool: poolCfg(),
	})
	if err != nil {
		t.Fatalf("fresh run: %v", err)
	}
	if len(res2.Faults) != len(res3.Faults) {
		t.Fatalf("fault count differs: %d vs %d", len(res2.Faults), len(res3.Faults))
	}
	// The decisive acceptance property: identical SHA-256 across the two
	// complete runs despite different execution paths (resume vs straight).
	if res2.Summary.SHA256 != res3.Summary.SHA256 {
		t.Fatalf("complete-run digest mismatch: resume=%q straight=%q",
			res2.Summary.SHA256, res3.Summary.SHA256)
	}
	for i := range res3.Faults {
		if fault.OrderKey(res2.Faults[i], res3.Faults[i]) != 0 {
			t.Fatalf("merged order differs at %d", i)
		}
	}
}

func TestResumeRejectsTamperedInput(t *testing.T) {
	spec := newTestSpec(400)
	media, _ := Build(spec)
	cp := filepath.Join(t.TempDir(), "cp.json")
	eng, _ := NewEngine(media)
	if _, err := eng.Run(context.Background(), RunConfig{
		Media: media, Checkpoint: cp, CancelAfter: 200, Pool: poolCfg(),
	}); err != nil {
		t.Fatal(err)
	}
	// A different seed changes the input digest; resume must be refused.
	tampered := spec
	tampered.Seed = 43
	tamMedia, _ := Build(tampered)
	eng2, _ := NewEngine(tamMedia)
	if _, err := eng2.Run(context.Background(), RunConfig{
		Media: tamMedia, Checkpoint: cp, Resume: true, Pool: poolCfg(),
	}); err == nil {
		t.Fatal("expected checkpoint mismatch error")
	}
}
