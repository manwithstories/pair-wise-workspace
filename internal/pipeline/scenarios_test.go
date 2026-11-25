package pipeline

import (
	"context"
	"testing"

	"avpipe/internal/metrics"
)

// TestConsecutiveLossSample exercises the burst-loss sample: consecutive
// media packets exceed single-packet FEC repair and must surface as
// unrecoverable gaps while the run still clears the recovery threshold.
func TestConsecutiveLossSample(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Frames = 300
	cfg.Seed = 7
	cfg.LossRate = 0.02
	cfg.Burst = 3
	rep, err := NewEngine(cfg).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.LostMedia == 0 {
		t.Fatal("expected media losses in burst sample")
	}
	// Bursts of 3+ cannot be repaired by single-loss XOR FEC.
	if rep.Unrecoverable == 0 {
		t.Fatal("consecutive loss produced no unrecoverable gaps")
	}
	verdict, _ := rep.Evaluate(metrics.DefaultThresholds(), true)
	if verdict != 0 {
		t.Fatalf("burst sample failed thresholds: %+v", rep)
	}
}

// TestCrossSubsystemFailure combines parser corruption, truncation and
// transport loss in one run and asserts the defects propagate into bad
// records, audio gaps, transport counters and the fallback flag together.
func TestCrossSubsystemFailure(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Frames = 240
	cfg.Seed = 7
	cfg.CorruptEvery = 40
	cfg.Truncate = 160
	cfg.LossRate = 0.04
	cfg.Burst = 1
	rep, err := NewEngine(cfg).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.BadRecords == 0 {
		t.Fatal("parser corruption not reported")
	}
	if !rep.IndexFallback {
		t.Fatal("truncation did not trigger index fallback")
	}
	if rep.AudioGaps == 0 {
		t.Fatal("frame gaps did not propagate to audio")
	}
	if rep.LostPackets == 0 {
		t.Fatal("transport loss not observed")
	}
	if rep.ParserErrors == 0 {
		t.Fatal("parser produced no locatable errors")
	}
	verdict, reasons := rep.Evaluate(metrics.DefaultThresholds(), true)
	if verdict != 0 {
		t.Fatalf("cross-subsystem run failed: %v report=%+v", reasons, rep)
	}
}
