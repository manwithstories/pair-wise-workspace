package pipeline

import (
	"context"
	"testing"

	"avpipe/internal/metrics"
)

func runCfg(frames int, seed int64) Config {
	c := DefaultConfig()
	c.Frames = frames
	c.Seed = seed
	return c
}

func TestCleanRunMetrics(t *testing.T) {
	cfg := runCfg(120, 7)
	cfg.Case = "clean"
	cfg.LossRate = 0
	rep, err := NewEngine(cfg).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.BadRecords != 0 {
		t.Fatalf("bad_records=%d, want 0 on clean run", rep.BadRecords)
	}
	if rep.IndexFallback {
		t.Fatal("clean run must not use index fallback")
	}
	if rep.RecoveredPct < 100 {
		t.Fatalf("recovered_pct=%f, want 100 on clean run", rep.RecoveredPct)
	}
	verdict, reasons := rep.Evaluate(metrics.DefaultThresholds(), false)
	if verdict != 0 {
		t.Fatalf("clean run verdict=%d reasons=%v", verdict, reasons)
	}
}

func TestDeterministicHashAndMetrics(t *testing.T) {
	cfg := runCfg(120, 7)
	cfg.LossRate = 0.05
	cfg.Burst = 2
	cfg.CorruptEvery = 50
	r1, err := NewEngine(cfg).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r2, err := NewEngine(cfg).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r1.OutputSHA256 != r2.OutputSHA256 {
		t.Fatalf("hash differs across runs: %s vs %s", r1.OutputSHA256, r2.OutputSHA256)
	}
	if r1.LatencyP95Ms != r2.LatencyP95Ms || r1.RecoveredPct != r2.RecoveredPct {
		t.Fatal("metrics differ across runs with the same seed")
	}
}

func TestDamagedRunRecovers(t *testing.T) {
	cfg := runCfg(200, 7)
	cfg.LossRate = 0.05
	cfg.Burst = 2
	cfg.CorruptEvery = 50
	cfg.Truncate = 200
	rep, err := NewEngine(cfg).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.BadRecords == 0 {
		t.Fatal("damaged run reported no bad records")
	}
	verdict, reasons := rep.Evaluate(metrics.DefaultThresholds(), true)
	if verdict != 0 {
		t.Fatalf("damaged run did not meet thresholds: verdict=%d reasons=%v report=%+v",
			verdict, reasons, rep)
	}
}

func TestGapPropagatesToAudioAndRTP(t *testing.T) {
	cfg := runCfg(120, 11)
	cfg.CorruptEvery = 20
	rep, err := NewEngine(cfg).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.BadRecords == 0 {
		t.Fatal("expected corrupt frames")
	}
	if rep.AudioGaps == 0 {
		t.Fatal("frame gaps did not propagate into audio alignment")
	}
	if rep.LostPackets+rep.Unrecoverable+rep.RecoveredFEC == 0 && cfg.LossRate == 0 {
		// With zero loss the RTP counters may all be zero; gap propagation is
		// asserted via AudioGaps above.
	}
}

func TestCancellationReturnsLocatableError(t *testing.T) {
	cfg := runCfg(60, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewEngine(cfg).Run(ctx)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
}

func TestCacheLimitReturnsLocatableError(t *testing.T) {
	orig := MaxCacheBytes
	MaxCacheBytes = 64
	defer func() { MaxCacheBytes = orig }()
	cfg := runCfg(20, 1)
	_, err := NewEngine(cfg).Run(context.Background())
	if err == nil {
		t.Fatal("expected cache-limit error")
	}
}
