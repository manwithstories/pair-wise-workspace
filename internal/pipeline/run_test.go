package pipeline

import (
	"testing"

	"audiorestore/internal/model"
	"audiorestore/internal/timeline"
	"audiorestore/internal/wav"
)

const (
	tBlockFrames = 64
	tDataBlocks  = 128
	tFrames      = tBlockFrames * tDataBlocks
)

func baseCfg() wav.GenerateConfig {
	return wav.GenerateConfig{
		SampleRate: 48000, Channels: 2, BitDepth: 16,
		Frames: tFrames, BlockFrames: tBlockFrames,
		Layout: "stereo_lr", Color: "bt709", Seed: 17,
	}
}

func anchors() []timeline.Anchor {
	return []timeline.Anchor{
		{ID: "a0", RefRate: model.SampleRate(48000), RefSample: 0},
		{ID: "a1", RefRate: model.SampleRate(48000), RefSample: 4800},
	}
}

func cfg() Config {
	return Config{WindowFrames: 128, KeepPartial: true,
		SilenceThresh: 100, SilenceMinRun: 8}
}

func TestCleanPipeline(t *testing.T) {
	raw, err := wav.Generate(baseCfg())
	if err != nil {
		t.Fatal(err)
	}
	run := New([]Carrier{{WAV: raw, ID: "t"}}, anchors(), 48000, cfg())
	s, err := run.Execute()
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if s.Degraded {
		t.Fatalf("clean run must not be degraded: %v", s.Errors)
	}
	if s.Segments == 0 {
		t.Fatal("expected segments")
	}
	if s.AVSyncP95US > 1000 {
		t.Fatalf("sync P95 %d us exceeds 1000 us budget", s.AVSyncP95US)
	}
	if s.LossRecoveryPct != 100.0 {
		t.Fatalf("clean recovery = %f, want 100", s.LossRecoveryPct)
	}
	if len(s.UnrecoveredBlocks) != 0 {
		t.Fatalf("clean run reported unrecovered blocks: %v", s.UnrecoveredBlocks)
	}
}

func TestSingleLossRecoveryHundredPercent(t *testing.T) {
	gen := baseCfg()
	gen.DropBlocks = []int{5}
	raw, _, err := wav.GenerateParity(gen, 4)
	if err != nil {
		t.Fatal(err)
	}
	run := New([]Carrier{{WAV: raw, ID: "t", DataPerGroup: 4}},
		anchors(), 48000, cfg())
	s, err := run.Execute()
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if s.LossRecoveryPct != 100.0 {
		t.Fatalf("recovery = %f, want 100", s.LossRecoveryPct)
	}
	if len(s.RecoveredBlocks) != 1 || s.RecoveredBlocks[0] != 5 {
		t.Fatalf("expected block 5 recovered, got %v", s.RecoveredBlocks)
	}
	if s.Degraded {
		t.Fatal("a fully recovered run must not stay degraded")
	}
}

func TestExcessLossReportsGaps(t *testing.T) {
	gen := baseCfg()
	gen.DropBlocks = []int{0, 1} // same parity group, two losses
	raw, _, err := wav.GenerateParity(gen, 4)
	if err != nil {
		t.Fatal(err)
	}
	run := New([]Carrier{{WAV: raw, ID: "t", DataPerGroup: 4}},
		anchors(), 48000, cfg())
	s, err := run.Execute()
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if s.LossRecoveryPct != 0.0 {
		t.Fatalf("recovery = %f, want 0 for excess loss", s.LossRecoveryPct)
	}
	if len(s.UnrecoveredBlocks) != 2 {
		t.Fatalf("expected 2 unrecovered blocks, got %v", s.UnrecoveredBlocks)
	}
	if !s.Degraded {
		t.Fatal("excess loss must degrade the run")
	}
}

func TestNoCompleteFramesReturnsTrackedError(t *testing.T) {
	gen := baseCfg()
	gen.DropBlocks = allIndices(tDataBlocks)
	raw, _, err := wav.GenerateParity(gen, 4)
	if err != nil {
		t.Fatal(err)
	}
	run := New([]Carrier{{WAV: raw, ID: "t", DataPerGroup: 4}},
		anchors(), 48000, cfg())
	_, err = run.Execute()
	if err == nil {
		t.Fatal("expected no-frames error")
	}
	if te, ok := err.(model.TrackedError); !ok || te.Kind != model.ErrNoFrames {
		t.Fatalf("expected ErrNoFrames, got %v", err)
	}
}

func TestDeterministicAcrossRuns(t *testing.T) {
	raw, err := wav.Generate(baseCfg())
	if err != nil {
		t.Fatal(err)
	}
	run1 := New([]Carrier{{WAV: raw, ID: "t"}}, anchors(), 48000, cfg())
	s1, err := run1.Execute()
	if err != nil {
		t.Fatal(err)
	}
	run2 := New([]Carrier{{WAV: raw, ID: "t"}}, anchors(), 48000, cfg())
	s2, err := run2.Execute()
	if err != nil {
		t.Fatal(err)
	}
	// pipeline_p95_ms is a wall-clock measurement and is allowed to vary;
	// every content-derived field must be byte-identical across runs.
	s1.PipelineP95MS, s2.PipelineP95MS = 0, 0
	if s1.RenderText() != s2.RenderText() {
		t.Fatal("identical seeds must produce identical content summaries")
	}
}

func allIndices(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}
