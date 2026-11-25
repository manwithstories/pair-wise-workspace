package pipeline_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"avcheck/internal/media"
	"avcheck/internal/pipeline"
	"avcheck/internal/transport"
)

func testConfig(t *testing.T) pipeline.Config {
	t.Helper()
	cfg := pipeline.DefaultConfig()
	cfg.Seed = 731
	cfg.TargetBps = 900_000
	cfg.Link = transport.LinkConfig{GroupSize: 16, JitterNs: 15_000_000}
	return cfg
}

func buildCase(t *testing.T, name pipeline.Case) ([]byte, pipeline.Scenario) {
	t.Helper()
	sc, err := pipeline.ScenarioFor(name, 731, 900_000, transport.HardMaxBufferBytes)
	if err != nil {
		t.Fatal(err)
	}
	// Fixture construction lives behind the CLI in production; rebuild it
	// through the same exported path the scenario uses.
	data, err := pipeline.BuildScenario(sc)
	if err != nil {
		t.Fatal(err)
	}
	return data, sc
}

// TestCleanRunCommits verifies a clean sample commits a manifest whose
// metrics meet the acceptance thresholds.
func TestCleanRunCommits(t *testing.T) {
	data, _ := buildCase(t, pipeline.CaseClean)
	cfg := testConfig(t)
	runner, err := pipeline.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	res, err := runner.RunFromBytes(context.Background(), data, dir)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Committed || res.Manifest == nil {
		t.Fatalf("clean run not committed: %+v", res)
	}
	m := res.Manifest
	if m.DeltaPct > 5 || m.DeltaPct < -5 {
		t.Fatalf("delta_pct = %.2f", m.DeltaPct)
	}
	if m.AVSkewP95Ms > 20 {
		t.Fatalf("av skew p95 = %.2f ms", m.AVSkewP95Ms)
	}
	if m.ManifestDeltaMs > 2 {
		t.Fatalf("manifest delta = %.2f ms", m.ManifestDeltaMs)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
}

// TestTruncatedVintPropagatesAcrossModules requires the parser's truncation
// to stop the pipeline before sync, commit diagnostics only, and expose exit
// code 2 through the fault model.
func TestTruncatedVintPropagatesAcrossModules(t *testing.T) {
	data, _ := buildCase(t, pipeline.CaseTruncatedVint)
	cfg := testConfig(t)
	runner, err := pipeline.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	res, err := runner.RunFromBytes(context.Background(), data, dir)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Committed {
		t.Fatal("truncated input must not commit a manifest")
	}
	if !res.Faults.Has(media.CodeTruncatedVint) {
		t.Fatalf("faults = %v, want truncated_vint", res.Faults)
	}
	if got := media.ExitCode(res.Faults); got != media.ExitCorrupt {
		t.Fatalf("exit code = %d, want %d", got, media.ExitCorrupt)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); !os.IsNotExist(err) {
		t.Fatal("manifest.json must not exist for a truncated input")
	}
	if _, err := os.Stat(filepath.Join(dir, "diagnostics.txt")); err != nil {
		t.Fatalf("diagnostics missing: %v", err)
	}
}

// TestDoubleLossPropagatesToManifest requires two losses in one group to
// survive transport, reach the sync gaps, and show up in the manifest as
// unrecovered rather than as a repaired packet.
func TestDoubleLossPropagatesToManifest(t *testing.T) {
	data, sc := buildCase(t, pipeline.CaseDoubleLoss)
	cfg := testConfig(t)
	cfg.Link = sc.Link
	runner, err := pipeline.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := runner.RunFromBytes(context.Background(), data, t.TempDir())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Committed || res.Manifest == nil {
		t.Fatalf("double-loss run should still commit: %+v", res)
	}
	if res.Manifest.Quality.Unrecovered == 0 {
		t.Fatal("expected unrecovered packets from the double-loss group")
	}
	if !res.Faults.Has(media.CodePacketGap) {
		t.Fatalf("faults = %v, want packet_gap", res.Faults)
	}
	if !res.Faults.Has(media.CodeRepairOverflow) {
		t.Fatalf("faults = %v, want repair_overflow", res.Faults)
	}
	// The manifest must distinguish recovered from unrecovered, both in its
	// counters and in its fault list.
	if res.Manifest.Quality.RecoveredPackets != res.Manifest.RecoveredPackets {
		t.Fatal("recovered packet counts disagree")
	}
	codes := map[string]int{}
	for _, f := range res.Manifest.Faults {
		codes[f.Code]++
	}
	if codes[string(media.CodePacketGap)] == 0 {
		t.Fatalf("manifest faults lack packet_gap: %+v", codes)
	}
	if codes[string(media.CodePacketRecovered)] == 0 {
		t.Fatalf("manifest faults lack packet_recovered: %+v", codes)
	}
}

// TestMemoryBudgetIsRespected requires the peak queue to stay under the
// caller's budget.
func TestMemoryBudgetIsRespected(t *testing.T) {
	data, sc := buildCase(t, pipeline.CaseBurstReorderSlow)
	cfg := testConfig(t)
	cfg.Link = sc.Link
	cfg.MemoryBytes = 1 << 20
	runner, err := pipeline.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := runner.RunFromBytes(context.Background(), data, t.TempDir())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Committed {
		t.Fatalf("run not committed: %v", res.Faults)
	}
	if res.Manifest.PeakBufferBytes > 1<<20 {
		t.Fatalf("peak buffer %d exceeds budget %d", res.Manifest.PeakBufferBytes, 1<<20)
	}
}

// TestCanceledRunReleasesWork requires a canceled context to abort without a
// manifest and without leaking a manifest into the output directory.
func TestCanceledRunReleasesWork(t *testing.T) {
	data, _ := buildCase(t, pipeline.CaseClean)
	cfg := testConfig(t)
	runner, err := pipeline.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := t.TempDir()
	_, err = runner.RunFromBytes(ctx, data, dir)
	if err == nil {
		t.Fatal("expected a cancellation error")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "manifest.json")); !os.IsNotExist(statErr) {
		t.Fatal("canceled run left a manifest")
	}
}

// TestUnknownTrackCaseAborts requires a dependency on an undeclared track to
// stop the run with diagnostics, not a partial manifest.
func TestUnknownTrackCaseAborts(t *testing.T) {
	data, _ := buildCase(t, pipeline.CaseUnknownTrack)
	cfg := testConfig(t)
	runner, err := pipeline.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := runner.RunFromBytes(context.Background(), data, t.TempDir())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Committed {
		t.Fatal("unknown track must not commit")
	}
	if !res.Faults.Has(media.CodeUnknownTrack) {
		t.Fatalf("faults = %v, want unknown_track", res.Faults)
	}
}

// TestCycleCaseAborts requires a dependency cycle to be a hard error.
func TestCycleCaseAborts(t *testing.T) {
	data, _ := buildCase(t, pipeline.CaseCycle)
	cfg := testConfig(t)
	runner, err := pipeline.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := runner.RunFromBytes(context.Background(), data, t.TempDir())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Committed {
		t.Fatal("dependency cycle must not commit")
	}
	if !res.Faults.Has(media.CodeDependencyCycle) {
		t.Fatalf("faults = %v, want dependency_cycle", res.Faults)
	}
}

func TestConfigRejectsUnsafeLimits(t *testing.T) {
	cfg := pipeline.DefaultConfig()
	cfg.MemoryBytes = transport.HardMaxBufferBytes + 1
	if err := cfg.Validate(); err == nil {
		t.Fatal("memory budget above 8 MiB must be rejected")
	}
	cfg = pipeline.DefaultConfig()
	cfg.TargetBps = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("zero target bps must be rejected")
	}
}

// TestNoManifestForOversizeInput requires the oversize element case to be
// refused before any allocation and to commit diagnostics only.
func TestNoManifestForOversizeInput(t *testing.T) {
	data, _ := buildCase(t, pipeline.CaseOversize)
	cfg := testConfig(t)
	runner, err := pipeline.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := runner.RunFromBytes(context.Background(), data, t.TempDir())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Committed {
		t.Fatal("oversize element must not commit")
	}
	if !res.Faults.Has(media.CodeElementTooLarge) {
		t.Fatalf("faults = %v, want element_too_large", res.Faults)
	}
}

var _ = bytes.Equal
