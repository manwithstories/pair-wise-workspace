package pipeline

import (
	"context"
	"testing"

	"mediaflow/internal/report"
)

func TestCleanCaseEndToEnd(t *testing.T) {
	out, err := Run(context.Background(), CaseClean, Config{
		Seed: 41, Root: t.TempDir(), ReorderWindow: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	gate := out.Metrics.Evaluate(report.DefaultThresholds(), false)
	if !gate.Pass {
		t.Fatalf("clean case failed gates: %+v metrics=%+v", gate, out.Metrics)
	}
	if !out.Metrics.OutputShaMatch {
		t.Fatal("clean output hash must match")
	}
	if len(out.Normalized) == 0 || out.Index == nil || out.MetaReport == nil {
		t.Fatal("pipeline outputs incomplete")
	}
}

func TestDamageCaseReportsGaps(t *testing.T) {
	out, err := Run(context.Background(), CaseDamage, Config{
		Seed: 41, Root: t.TempDir(), ReorderWindow: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Gaps) == 0 || out.Metrics.Gaps == 0 {
		t.Fatal("damage case must report unrecovered gaps")
	}
	if out.Metrics.Lost <= out.Metrics.Recovered {
		t.Fatalf("damage case expects unrecovered loss: lost=%d recovered=%d",
			out.Metrics.Lost, out.Metrics.Recovered)
	}
	if out.Metrics.Truncated != 2 {
		t.Fatalf("expected 2 truncated events, got %d", out.Metrics.Truncated)
	}
}

func TestResumeCaseHashMatches(t *testing.T) {
	out, err := Run(context.Background(), CaseResume, Config{
		Seed: 41, Root: t.TempDir(), ReorderWindow: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	gate := out.Metrics.Evaluate(report.DefaultThresholds(), false)
	if !gate.Pass {
		t.Fatalf("resume case failed gates: %+v metrics=%+v", gate, out.Metrics)
	}
	if !out.Metrics.OutputShaMatch {
		t.Fatal("resume output hash must match clean encoding")
	}
}

func TestDeterministicAcrossRuns(t *testing.T) {
	cfg1 := Config{Seed: 41, Root: t.TempDir(), ReorderWindow: 16}
	cfg2 := Config{Seed: 41, Root: t.TempDir(), ReorderWindow: 16}
	a, err := Run(context.Background(), CaseClean, cfg1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Run(context.Background(), CaseClean, cfg2)
	if err != nil {
		t.Fatal(err)
	}
	if a.Metrics.Index != b.Metrics.Index || a.StreamStats.OutputSHA != b.StreamStats.OutputSHA {
		t.Fatal("identical seed produced non-deterministic results")
	}
}
