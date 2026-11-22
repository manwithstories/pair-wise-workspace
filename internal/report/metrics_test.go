package report

import (
	"math"
	"testing"

	"mediacheck/internal/model"
)

func TestBuildMetricsAndBitrateWindow(t *testing.T) {
	in := Inputs{
		Seed: 7, TargetKBPS: 6000,
		Normalize: model.NormalizeSnapshot{
			SamplesIn: 88200, SamplesOut: 96000, PeakDBFS: -1.5,
			IntegratedLUFS: -23.1, SilentFrames: 3,
		},
		Pack:    model.PackSnapshot{ActualKBPS: 5980},
		Color:   model.ColorSnapshot{Conflicts: 2},
		AlignMS: 0.4, Gaps: 1,
	}
	m := Build(in)
	if m.Status != "ok" {
		t.Fatalf("status = %s", m.Status)
	}
	if m.ActualKBPS < 5940 || m.ActualKBPS > 6060 {
		t.Fatalf("actual kbps outside acceptance window: %d", m.ActualKBPS)
	}
	if m.BitrateDeltaPct > 1.0 {
		t.Fatalf("delta pct = %f", m.BitrateDeltaPct)
	}
	if m.SamplesIn != 88200 || m.SamplesOut != 96000 {
		t.Fatal("sample counts missing")
	}
	if m.SummarySHA == "" || len(m.SummarySHA) != 64 {
		t.Fatalf("summary sha = %q", m.SummarySHA)
	}
}

func TestSummarySHADeterministic(t *testing.T) {
	in := Inputs{Seed: 1, TargetKBPS: 6000, Pack: model.PackSnapshot{ActualKBPS: 6000}}
	a := Build(in)
	b := Build(in)
	if a.SummarySHA != b.SummarySHA {
		t.Fatal("identical inputs must yield identical summary sha")
	}
	in.Seed = 2
	c := Build(in)
	if c.SummarySHA == a.SummarySHA {
		t.Fatal("different inputs must change summary sha")
	}
}

func TestNoNaNEntersSummary(t *testing.T) {
	in := Inputs{
		Seed: 1, TargetKBPS: 6000,
		Normalize: model.NormalizeSnapshot{PeakDBFS: math.NaN(), IntegratedLUFS: math.Inf(1)},
		Pack: model.PackSnapshot{ActualKBPS: 6000},
	}
	m := Build(in)
	if math.IsNaN(m.PeakDBFS) || math.IsInf(m.LUFS, 0) || math.IsNaN(m.BitrateDeltaPct) {
		t.Fatal("non-finite values leaked into metrics")
	}
}

func TestRejectMetrics(t *testing.T) {
	m := Reject(7, model.ErrTFDTRegression)
	if m.Status != "rejected" || m.RejectReason != "tfdt_regression" {
		t.Fatalf("reject metrics = %+v", m)
	}
	if m.SummarySHA == "" {
		t.Fatal("rejected summary still needs stable sha")
	}
}
