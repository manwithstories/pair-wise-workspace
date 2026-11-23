package pipeline

import (
	"errors"
	"testing"
)

func baseCfg() Config {
	return Config{Seed: 42, DurationSec: 120, LossBurst: 8, Rates: 3}
}

// TestRunRecoveredPath is the acceptance scenario: loss burst and timestamp
// wrap are injected and fully recovered.
func TestRunRecoveredPath(t *testing.T) {
	m, err := Run(baseCfg())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if m.SyncErrorMS > 20 {
		t.Errorf("sync_error_ms=%v > 20", m.SyncErrorMS)
	}
	if m.LossRecoveryRate < 0.90 {
		t.Errorf("loss_recovery_rate=%v < 0.90", m.LossRecoveryRate)
	}
	if m.IndexEntries < 100 {
		t.Errorf("index_entries=%d < 100", m.IndexEntries)
	}
	if m.BoundaryMismatches != 0 {
		t.Errorf("boundary_mismatches=%d", m.BoundaryMismatches)
	}
	if m.P95LatencyMS > 250 {
		t.Errorf("p95_latency_ms=%v > 250", m.P95LatencyMS)
	}
	if m.RecoveredSegments < 1 {
		t.Errorf("recovered_segments=%d < 1", m.RecoveredSegments)
	}
	if m.CorruptedRanges < 1 {
		t.Errorf("corrupted_ranges=%d, want the recovered burst recorded", m.CorruptedRanges)
	}
	if m.UnrecoveredRanges != 0 {
		t.Errorf("unrecovered_ranges=%d", m.UnrecoveredRanges)
	}
	if len(m.ColorConflicts) == 0 {
		t.Error("expected color conflicts to be recorded")
	}
}

// TestRunCorruptedPath drops the sender buffer so the burst cannot be
// recovered: located errors propagate and ErrUnrecovered is returned.
func TestRunCorruptedPath(t *testing.T) {
	cfg := baseCfg()
	cfg.Unrecoverable = true
	m, err := Run(cfg)
	if !errors.Is(err, ErrUnrecovered) {
		t.Fatalf("err=%v, want ErrUnrecovered", err)
	}
	if m.UnrecoveredRanges < 1 {
		t.Error("want unrecovered ranges")
	}
	if m.SyncErrorMS > 40 {
		t.Errorf("sync_error_ms=%v > 40 on corrupted sample", m.SyncErrorMS)
	}
	found := false
	for _, f := range m.Faults {
		if len(f) > 4 && f[:4] == "rtp:" {
			found = true
		}
	}
	if !found {
		t.Error("want located rtp fault in report")
	}
}

// TestRunNormalPath has no injected loss: nothing to recover, exit clean.
func TestRunNormalPath(t *testing.T) {
	cfg := baseCfg()
	cfg.LossBurst = 0
	m, err := Run(cfg)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if m.LossRecoveryRate != 1 || m.UnrecoveredRanges != 0 {
		t.Errorf("metrics=%+v", m)
	}
}

func TestRunDeterministic(t *testing.T) {
	a, _ := Run(baseCfg())
	b, _ := Run(baseCfg())
	if a.IndexDigest != b.IndexDigest {
		t.Fatalf("digests differ:\n%s\n%s", a.IndexDigest, b.IndexDigest)
	}
}

func TestRunTruncatedStream(t *testing.T) {
	cfg := baseCfg()
	cfg.InjectTruncate = true
	m, err := Run(cfg)
	if !errors.Is(err, ErrUnrecovered) {
		t.Fatalf("err=%v, want ErrUnrecovered", err)
	}
	if m.CorruptedRanges == 0 {
		t.Error("truncation must produce a corrupt range")
	}
}

func TestConfigValidate(t *testing.T) {
	if err := baseCfg().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Config{
		{DurationSec: 0, Rates: 3}, {DurationSec: 99999, Rates: 3},
		{DurationSec: 10, Rates: 2}, {DurationSec: 10, Rates: 3, LossBurst: -1},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("cfg=%+v must be rejected", bad)
		}
	}
}
