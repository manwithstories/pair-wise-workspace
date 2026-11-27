package pipeline

import (
	"context"
	"errors"
	"testing"

	"archive/internal/manifest"
	"archive/internal/model"
	"archive/internal/report"
)

func baseConfig(kind CaseKind) Config {
	return Config{
		Kind: kind, Format: manifest.FormatHLS, Seed: 41, TargetKbps: 1800,
		BandwidthKbps:  []float64{900, 4200, 1200},
		DuplicateEvery: 7, ReorderWindow: 9, LossBurst: 4,
		MemoryBytes: 8388608,
	}
}

func TestRunNormalCase(t *testing.T) {
	cfg := baseConfig(CaseNormal)
	s, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("normal run: %v", err)
	}
	if s.Status != report.StatusOK || s.Gaps != 0 {
		t.Fatalf("normal case must be gap-free, got %+v", s)
	}
	if s.QualityCRCPct != 100.0 {
		t.Fatalf("normal CRC = %.2f, want 100", s.QualityCRCPct)
	}
	if s.SyncErrorMs > 20.0 {
		t.Fatalf("sync error %.2f exceeds 20ms", s.SyncErrorMs)
	}
	if s.PeakBytes > int64(cfg.MemoryBytes) {
		t.Fatalf("peak %d exceeds memory %d", s.PeakBytes, cfg.MemoryBytes)
	}
}

func TestRunRepairedCaseFullyRecovered(t *testing.T) {
	cfg := baseConfig(CaseRepaired)
	s, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("repaired run: %v", err)
	}
	if s.Status != report.StatusOK || s.Gaps != 0 {
		t.Fatalf("single losses must all repair, got status=%s gaps=%d", s.Status, s.Gaps)
	}
	if s.Recovered == 0 {
		t.Fatal("repaired case must count recovered packets")
	}
	if s.QualityCRCPct != 100.0 {
		t.Fatalf("repaired CRC = %.2f, want 100", s.QualityCRCPct)
	}
}

func TestRunDamagedCaseDegrades(t *testing.T) {
	cfg := baseConfig(CaseDamaged)
	s, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("damaged run should finish, got %v", err)
	}
	if s.Status != report.StatusDegraded || s.Gaps == 0 {
		t.Fatalf("damaged case must show gaps and degraded, got %+v", s)
	}
	if s.PeakBytes > int64(cfg.MemoryBytes) {
		t.Fatalf("peak %d exceeds memory limit", s.PeakBytes)
	}
}

func TestStrictModeExitTwo(t *testing.T) {
	cfg := baseConfig(CaseDamaged)
	cfg.Strict = true
	s, err := Run(context.Background(), cfg)
	if !errors.Is(err, model.ErrStrictUnrecovered) {
		t.Fatalf("strict damaged run must return ErrStrictUnrecovered, got %v", err)
	}
	if s.Gaps == 0 {
		t.Fatal("strict summary must still carry the gap count")
	}
}

func TestInvalidConfigRejected(t *testing.T) {
	cfg := baseConfig(CaseNormal)
	cfg.MemoryBytes = model.MaxQueueBytes + 1
	if _, err := Run(context.Background(), cfg); !errors.Is(err, model.ErrInvariant) {
		t.Fatalf("oversize memory must be invariant error, got %v", err)
	}
	cfg = baseConfig(CaseNormal)
	cfg.TargetKbps = 0
	if _, err := Run(context.Background(), cfg); !errors.Is(err, model.ErrInvariant) {
		t.Fatalf("zero target must be invariant error, got %v", err)
	}
}

func TestInterruptResumesAndPreservesProgress(t *testing.T) {
	cfg := baseConfig(CaseNormal)
	cfg.InterruptAfter = 20
	s, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("interrupted run: %v", err)
	}
	if s.Status != report.StatusOK {
		t.Fatalf("resumed run must still complete cleanly, got %+v", s)
	}
	// A second, uninterrupted run must yield the identical semantic digest.
	cfg2 := baseConfig(CaseNormal)
	s2, err := Run(context.Background(), cfg2)
	if err != nil {
		t.Fatalf("control run: %v", err)
	}
	if s.SemanticSHA != s2.SemanticSHA {
		t.Fatal("interrupt/resume changed the semantic outcome")
	}
}

func TestDeterministicSemanticDigest(t *testing.T) {
	cfg := baseConfig(CaseRepaired)
	s1, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	s2, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if s1.SemanticSHA != s2.SemanticSHA {
		t.Fatal("same seed produced different semantic digests")
	}
}
