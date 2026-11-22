package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mediacheck/internal/fixture"
	"mediacheck/internal/model"
)

func testCfg(out string, inj fixture.Injection, policy RegressionPolicy) PipelineConfig {
	return PipelineConfig{
		Seed: 7, Rate: 48000, Layout: model.LayoutStereo,
		TargetRate: 48000, TargetLayout: model.LayoutStereo,
		TargetLUFS: -23, PeakLimit: 0.95, TargetKBPS: 6000,
		Injection: inj, Policy: policy, OutDir: out,
		ColorPriority: ColorPriority{
			Order:   []string{"9/16/9", "1/1/1"},
			Default: model.ColorMeta{ColorPrimaries: 1, Transfer: 1, Matrix: 1},
		},
		FillSilence: true,
	}
}

func TestPipelineHappyPath(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	res, err := Run(context.Background(), reg, testCfg(out, fixture.InjectNone, RegressionReject))
	if err != nil {
		t.Fatal(err)
	}
	if res.Rejected {
		t.Fatal("happy path must not be rejected")
	}
	m := res.Metrics
	if m.ActualKBPS < 5940 || m.ActualKBPS > 6060 {
		t.Fatalf("actual kbps = %d outside 5940-6060", m.ActualKBPS)
	}
	if m.BitrateDeltaPct > 1.0 {
		t.Fatalf("delta pct = %f", m.BitrateDeltaPct)
	}
	if m.FragmentGaps != 1 {
		t.Fatalf("fragment gaps = %d, want 1", m.FragmentGaps)
	}
	if m.ManifestAlignMS > 1.0 {
		t.Fatalf("align ms = %f", m.ManifestAlignMS)
	}
	if m.SamplesIn == 0 || m.SamplesOut == 0 {
		t.Fatal("sample counts missing")
	}
	if _, err := os.Stat(filepath.Join(out, "manifest.json")); err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
}

func TestPipelineRejectsTFDTRegression(t *testing.T) {
	reg, _ := DefaultRegistry()
	out := t.TempDir()
	res, err := Run(context.Background(), reg, testCfg(out, fixture.InjectTFDTRegression, RegressionReject))
	if err == nil {
		t.Fatal("tfdt regression must error")
	}
	if !res.Rejected || res.Metrics.RejectReason != "tfdt_regression" {
		t.Fatalf("reject result = %+v", res)
	}
	// 损坏组不得产出成功清单。
	if _, err := os.Stat(filepath.Join(out, "manifest.json")); !os.IsNotExist(err) {
		t.Fatal("no success manifest allowed on rejection")
	}
}

func TestPipelineRebaseReproducible(t *testing.T) {
	reg, _ := DefaultRegistry()
	out1 := t.TempDir()
	res1, err := Run(context.Background(), reg, testCfg(out1, fixture.InjectTFDTRegression, RegressionRebase))
	if err != nil {
		t.Fatal(err)
	}
	out2 := t.TempDir()
	cfg2 := testCfg(out2, fixture.InjectTFDTRegression, RegressionRebase)
	res2, err := Run(context.Background(), reg, cfg2)
	if err != nil {
		t.Fatal(err)
	}
	if res1.Metrics.SummarySHA != res2.Metrics.SummarySHA {
		t.Fatalf("rebase rerun summary sha mismatch:\n%s\n%s",
			res1.Metrics.SummarySHA, res2.Metrics.SummarySHA)
	}
}

func TestPipelineRejectsCorruptions(t *testing.T) {
	reg, _ := DefaultRegistry()
	cases := []fixture.Injection{
		fixture.InjectMissingMFHD,
		fixture.InjectTruncatedBox,
		fixture.InjectUnknownLayout,
	}
	for _, inj := range cases {
		out := t.TempDir()
		res, err := Run(context.Background(), reg, testCfg(out, inj, RegressionReject))
		if err == nil || !res.Rejected {
			t.Fatalf("%s must be rejected", inj)
		}
	}
}
