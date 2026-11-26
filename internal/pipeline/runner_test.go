package pipeline_test

import (
	"errors"
	"testing"

	"avpipe/internal/pipeline"
)

func testOptions(t *testing.T, caseName string, interrupt int) pipeline.Options {
	t.Helper()
	return pipeline.Options{
		Out: t.TempDir(), Seed: 23, Case: caseName,
		Bitrates: []int64{600000, 1200000}, Segments: 12,
		InterruptAfter: interrupt,
	}
}

func TestCleanPipelineAcceptanceMetrics(t *testing.T) {
	opt := testOptions(t, "clean", 0)
	rep, err := pipeline.RunDemo(opt)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Packets != 1920 {
		t.Fatalf("packets=%d want 1920", rep.Packets)
	}
	if rep.KeyframeFound != 24 || rep.KeyframeExpected != 24 {
		t.Fatalf("keyframe=%d/%d", rep.KeyframeFound, rep.KeyframeExpected)
	}
	if rep.AlignedSlots != 12 || rep.GapSlots != 0 {
		t.Fatalf("aligned=%d gap=%d", rep.AlignedSlots, rep.GapSlots)
	}
	if rep.SegmentHashOK != 24 || rep.SegmentHashTotal != 24 {
		t.Fatalf("hashes=%d/%d", rep.SegmentHashOK, rep.SegmentHashTotal)
	}
	if got := rep.RoundTripPercent(); got != 100 {
		t.Fatalf("roundtrip=%.2f want 100", got)
	}
	verified, err := pipeline.RunVerify(pipeline.Options{Out: opt.Out})
	if err != nil {
		t.Fatal(err)
	}
	if verified.SegmentHashOK != 24 || verified.RoundTripPercent() != 100 {
		t.Fatalf("verify hashes=%d/%d rt=%.2f", verified.SegmentHashOK, verified.SegmentHashTotal, verified.RoundTripPercent())
	}
}

func TestDamagedPipelinePropagatesOneGapAndRejection(t *testing.T) {
	rep, err := pipeline.RunDemo(testOptions(t, "damaged", 0))
	if err != nil {
		t.Fatal(err)
	}
	if rep.CCGap != 1 || rep.PSICRCReject != 1 {
		t.Fatalf("cc=%d psi=%d", rep.CCGap, rep.PSICRCReject)
	}
	if rep.KeyframeFound != 23 || rep.KeyframeExpected != 24 {
		t.Fatalf("keyframe=%d/%d want 23/24", rep.KeyframeFound, rep.KeyframeExpected)
	}
	if rep.GapSlots != 1 {
		t.Fatalf("gap_slots=%d want 1", rep.GapSlots)
	}
}

func TestTruncatedPipelineReportsOneIncompleteTail(t *testing.T) {
	rep, err := pipeline.RunDemo(testOptions(t, "truncated", 0))
	if err != nil {
		t.Fatal(err)
	}
	if rep.IncompleteTail != 1 {
		t.Fatalf("incomplete_tail=%d want 1", rep.IncompleteTail)
	}
	for _, c := range rep.Chunks {
		for _, s := range c.Segments {
			if s.Size%188 != 0 {
				t.Fatalf("segment %s size=%d includes a partial TS packet", s.Path, s.Size)
			}
		}
	}
}

func TestInterruptResumeMetricsAndStableHashes(t *testing.T) {
	opt := testOptions(t, "clean", 8)
	rep, err := pipeline.RunDemo(opt)
	if !errors.Is(err, pipeline.ErrInterrupt) {
		t.Fatalf("demo err=%v want interrupt", err)
	}
	if len(rep.Chunks) != 8 {
		t.Fatalf("durable chunks=%d want 8", len(rep.Chunks))
	}
	before := map[string]string{}
	partialCount := 0
	for _, c := range rep.Chunks {
		for _, s := range c.Segments {
			if s.Partial {
				partialCount++
				continue
			}
			before[s.Path] = s.SHA256
		}
	}
	if partialCount != 32 {
		t.Fatalf("interrupted partial records=%d want 32", partialCount)
	}

	resumed, err := pipeline.RunResume(pipeline.Options{Out: opt.Out})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.CheckpointReused != 8 || resumed.Rebuilt != 4 || resumed.Idempotent != 1 {
		t.Fatalf("reused=%d rebuilt=%d idempotent=%d", resumed.CheckpointReused, resumed.Rebuilt, resumed.Idempotent)
	}
	partialCount = 0
	for _, c := range resumed.Chunks {
		for _, s := range c.Segments {
			if s.Partial {
				partialCount++
				continue
			}
			if old, ok := before[s.Path]; ok && old != s.SHA256 {
				t.Fatalf("hash changed for %s", s.Path)
			}
			before[s.Path] = s.SHA256
		}
	}
	if partialCount != 48 {
		t.Fatalf("partial records=%d want 48", partialCount)
	}
	if len(before) != 24 {
		t.Fatalf("unique hashed segments=%d want 24", len(before))
	}
	again, err := pipeline.RunResume(pipeline.Options{Out: opt.Out})
	if err != nil {
		t.Fatal(err)
	}
	if again.CheckpointReused != 12 || again.Rebuilt != 0 || again.Idempotent != 1 {
		t.Fatalf("second resume reused=%d rebuilt=%d idempotent=%d", again.CheckpointReused, again.Rebuilt, again.Idempotent)
	}
	verified, err := pipeline.RunVerify(pipeline.Options{Out: opt.Out})
	if err != nil {
		t.Fatal(err)
	}
	if verified.SegmentHashOK != 24 || verified.SegmentHashTotal != 24 {
		t.Fatalf("verify hashes=%d/%d", verified.SegmentHashOK, verified.SegmentHashTotal)
	}
}
