package pipeline

import (
	"testing"

	"archive.local/recover/internal/testkit"
)

// TestDemoAcceptance locks the acceptance thresholds for the default demo.
func TestDemoAcceptance(t *testing.T) {
	res, err := Run(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	m := res.Report.Metrics
	if m.SyncErrorMs > 20 {
		t.Errorf("sync_error_ms=%.3f exceeds 20 ms", m.SyncErrorMs)
	}
	if m.LossRecoveryRate < 0.90 {
		t.Errorf("loss_recovery_rate=%.4f below 0.90", m.LossRecoveryRate)
	}
	if m.P95LatencyMs > 500 {
		t.Errorf("p95_latency_ms=%.3f exceeds 500 ms", m.P95LatencyMs)
	}
	if m.Segments <= 0 {
		t.Errorf("segments=%d must be positive", m.Segments)
	}
	if m.AlignmentDeltaMs > 40 {
		t.Errorf("alignment_delta_ms=%.3f exceeds 40 ms", m.AlignmentDeltaMs)
	}
	if res.Report.ExitCode != 0 {
		t.Errorf("exit_code=%d want 0 (issues: %v)", res.Report.ExitCode, res.Report.IssueCounts)
	}
}

func TestCaseCorruptIndex(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Case = testkit.CaseCorruptIndex
	cfg.DurationNs = 60_000_000_000
	res, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Report.Index == nil {
		t.Fatal("no index report")
	}
	if res.Report.Index.Dropped == 0 {
		t.Fatalf("out-of-range index entry not dropped: %+v", res.Report.Index)
	}
	if res.Report.Index.Missing == 0 {
		t.Fatal("missing page not recovered")
	}
}

func TestCaseWrapVFRDrift(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Case = testkit.CaseWrapVFRDrift
	cfg.DurationNs = 60_000_000_000
	res, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Report.Metrics.SyncErrorMs > 20 {
		t.Errorf("sync_error_ms=%.3f exceeds 20 ms under wrap+VFR+drift", res.Report.Metrics.SyncErrorMs)
	}
	if res.Report.Metrics.Segments <= 0 {
		t.Error("no segments produced")
	}
}
