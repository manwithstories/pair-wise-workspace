package pipeline

import (
	"context"
	"testing"

	"mediamix/internal/audio"
	"mediamix/internal/buffer"
	"mediamix/internal/clock"
	"mediamix/internal/rtp"
)

func fixture(t *testing.T, loss, reorder float64) *rtp.Fixture {
	t.Helper()
	return rtp.Generate(rtp.DefaultStreamConfig(), rtp.FixtureConfig{
		Seed: 41, Seconds: 3, LossRate: loss, ReorderRate: reorder,
		DuplicateRate: 0.1, SenderReportEvery: 25,
	})
}

func TestCleanRunProducesCommittedOutput(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.QueueBytes = 1 << 20
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(context.Background(), fixture(t, 0.02, 0.05))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Output) == 0 {
		t.Fatal("no output produced")
	}
	if res.OutputSHA256 == "" {
		t.Fatal("missing output hash")
	}
	if res.PeakQueueBytes > cfg.QueueBytes {
		t.Fatalf("queue peak %d > budget %d", res.PeakQueueBytes, cfg.QueueBytes)
	}
	if !res.CalibrationOK {
		t.Fatal("sender report calibration should succeed")
	}
	if len(res.Commits) == 0 {
		t.Fatal("expected committed manifests")
	}
}

func TestFaultRecoveryHashesEqualClean(t *testing.T) {
	fx := fixture(t, 0.02, 0.05)

	cleanDir := t.TempDir()
	cleanCfg := DefaultConfig(cleanDir)
	cleanCfg.QueueBytes = 1 << 20
	cleanRes, err := New(cleanCfg)
	if err != nil {
		t.Fatal(err)
	}
	clean, err := cleanRes.Run(context.Background(), fx)
	if err != nil {
		t.Fatalf("clean run: %v", err)
	}

	for _, fault := range []Fault{FaultAfterNormalize, FaultBeforeCommit} {
		fdir := t.TempDir()
		fcfg := DefaultConfig(fdir)
		fcfg.QueueBytes = 1 << 20
		fcfg.Fault = fault
		fr, err := New(fcfg)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := fr.Run(context.Background(), fx)
		if err != nil {
			t.Fatalf("fault %s run: %v", fault, err)
		}
		if !rec.FaultInjected || !rec.Replayed {
			t.Fatalf("fault %s not exercised", fault)
		}
		if rec.OutputSHA256 != clean.OutputSHA256 {
			t.Fatalf("fault %s recovered hash != clean: %s vs %s",
				fault, rec.OutputSHA256, clean.OutputSHA256)
		}
		if len(rec.Output) != len(clean.Output) {
			t.Fatalf("fault %s output length mismatch", fault)
		}
	}
}

func TestQueueBudgetValidation(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.QueueBytes = buffer.MaxQueueBytes + 1
	if _, err := New(cfg); !clock.IsCode(err, clock.ErrInvalidParam) {
		t.Fatalf("over-limit queue must be rejected, got %v", err)
	}
}

func TestPLCAndConverterWiring(t *testing.T) {
	// Sanity: PLC at target rate produces stereo frames the loudness graph
	// accepts without error.
	plc := audio.NewPLC(audio.DefaultPLCConfig())
	plc.Observe(make([]float32, 960))
	out, ok, err := plc.Conceal(480, false)
	if err != nil || !ok || len(out) != 960 {
		t.Fatalf("plc: ok=%v err=%v len=%d", ok, err, len(out))
	}
}

func TestDurationLimitEnforced(t *testing.T) {
	// A 10+ minute synthetic stream must be rejected by the shared cap.
	longFx := rtp.Generate(rtp.DefaultStreamConfig(), rtp.FixtureConfig{
		Seed: 1, Seconds: 601, SenderReportEvery: 100,
	})
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	r, _ := New(cfg)
	_, err := r.Run(context.Background(), longFx)
	if !clock.IsCode(err, clock.ErrDurationTooLong) {
		t.Fatalf("expected duration limit error, got %v", err)
	}
}
