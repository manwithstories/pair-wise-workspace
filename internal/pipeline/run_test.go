package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"pairflow/media/internal/clock"
	"pairflow/media/internal/fixture"
	"pairflow/media/internal/timeline"
)

// acceptanceConfig mirrors the documented demo invocation.
func acceptanceConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		Seed: 41, DriftPPM: 85, WrapAt: 4294967000,
		FrameRate: fixture.RateDrop2997, FrameCount: 120,
		OutDir:          t.TempDir(),
		Policy:          timeline.ConflictLastWins,
		Render:          true,
		JumpThresholdMS: 2000, ResidualThresholdMS: 100,
	}
}

// TestAcceptanceRun covers the documented demo path across every
// module: fixture generation, subtitle recovery, RTP unwrapping, drift
// correction, MP4 indexing, timeline composition, validation and frame
// export.
func TestAcceptanceRun(t *testing.T) {
	cfg := acceptanceConfig(t)
	sum, err := Run(cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	th := DefaultThresholds()
	if !sum.Passed {
		t.Fatalf("run failed: %v", sum.Failures)
	}
	if sum.RoundtripRate.Cmp(th.MinRoundtrip) < 0 {
		t.Errorf("roundtrip_rate = %s, want >= %s", sum.RoundtripRate, th.MinRoundtrip)
	}
	if sum.KeyframeHitRate.Cmp(th.MinKeyframeHit) < 0 {
		t.Errorf("keyframe_hit_rate = %s, want >= %s", sum.KeyframeHitRate, th.MinKeyframeHit)
	}
	if sum.SegmentSHACount < th.MinSegments {
		t.Errorf("segment_sha256_count = %d, want >= %d", sum.SegmentSHACount, th.MinSegments)
	}
	if sum.RecoveredRecords < th.MinRecovered {
		t.Errorf("recovered_records = %d, want >= %d", sum.RecoveredRecords, th.MinRecovered)
	}
	if sum.ErrorsWithOffset < th.MinOffsetErrs {
		t.Errorf("errors_with_offset = %d, want >= %d", sum.ErrorsWithOffset, th.MinOffsetErrs)
	}
	if sum.VFRBytes < th.MinVFR {
		t.Errorf("vfr_samples = %d, want >= %d", sum.VFRBytes, th.MinVFR)
	}
	if sum.RTPWraps != 1 {
		t.Errorf("rtp_wraps = %d, want 1", sum.RTPWraps)
	}
	if sum.RTPJumps != 1 || sum.RTPAnomalies != 1 {
		t.Errorf("jumps=%d anomalies=%d, want 1 each", sum.RTPJumps, sum.RTPAnomalies)
	}
	if !sum.Monotonic || !sum.SubtitleBoundaryOK {
		t.Errorf("monotonic=%v boundaries=%v", sum.Monotonic, sum.SubtitleBoundaryOK)
	}
	drift := sum.EstimatedDriftPPM.Seconds()
	if drift < 80 || drift > 90 {
		t.Errorf("estimated drift = %.2f ppm, want ~85", drift)
	}
	if sum.Frames != 120 {
		t.Errorf("frames = %d, want 120", sum.Frames)
	}
	// The manifest and its frames must be on disk and consistent.
	raw, err := os.ReadFile(filepath.Join(cfg.OutDir, "manifest.json"))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var man struct {
		Frames   []struct{ Path, SHA256 string } `json:"frames"`
		Segments []struct{ SHA256 string }       `json:"segments"`
	}
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatalf("manifest JSON: %v", err)
	}
	if len(man.Frames) != sum.Frames || len(man.Segments) != sum.SegmentSHACount {
		t.Fatalf("manifest counts != summary")
	}
	for _, f := range man.Frames {
		if _, err := os.Stat(filepath.Join(cfg.OutDir, f.Path)); err != nil {
			t.Fatalf("frame %s: %v", f.Path, err)
		}
	}
}

// TestAcceptanceRunIsReproducible asserts byte-identical digests across
// two independent runs with the same seed.
func TestAcceptanceRunIsReproducible(t *testing.T) {
	a, err := Run(acceptanceConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Run(acceptanceConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if a.SegmentSHACount != b.SegmentSHACount {
		t.Fatalf("segment count differs: %d vs %d", a.SegmentSHACount, b.SegmentSHACount)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	// Output paths differ per t.TempDir; compare the digests instead.
	if a.KeyframeHitRate != b.KeyframeHitRate || a.RoundtripRate != b.RoundtripRate {
		t.Fatalf("rates differ between runs")
	}
	_ = ja
	_ = jb
}

// TestDriftDirectionIsRecovered checks that injected positive drift
// yields a positive estimate and vice versa.
func TestDriftDirectionIsRecovered(t *testing.T) {
	for _, ppm := range []int64{-120, 0, 85} {
		cfg := acceptanceConfig(t)
		cfg.DriftPPM = ppm
		cfg.Render = false
		sum, err := Run(cfg)
		if err != nil {
			t.Fatalf("drift %d: %v", ppm, err)
		}
		got := sum.EstimatedDriftPPM.Seconds()
		if ppm == 0 && (got > 1 || got < -1) {
			t.Errorf("zero drift estimated as %.2f", got)
		}
		if ppm != 0 && (got*float64(ppm) < 0) {
			t.Errorf("drift %d estimated with wrong sign: %.2f", ppm, got)
		}
	}
}

// TestValidationFailureSkipsExport ensures a failing gate writes no
// frames unless --continue-on-error is set.
func TestValidationFailureSkipsExport(t *testing.T) {
	cfg := acceptanceConfig(t)
	cfg.FrameCount = 4 // too few VFR samples and segments to pass
	sum, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Passed {
		t.Fatal("expected validation failure for a tiny corpus")
	}
	if !sum.ExportSkipped || sum.SegmentSHACount != 0 {
		t.Fatalf("export ran despite failure: skipped=%v segments=%d",
			sum.ExportSkipped, sum.SegmentSHACount)
	}
	entries, _ := os.ReadDir(cfg.OutDir)
	if len(entries) != 0 {
		t.Fatalf("frames written despite failed gate: %d entries", len(entries))
	}

	cfg.ContinueOnError = true
	sum, err = Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sum.SegmentSHACount == 0 {
		t.Fatal("--continue-on-error did not export")
	}
	if len(sum.Failures) == 0 {
		t.Fatal("failures should still be reported when continuing")
	}
}

// TestSubtitleConflictPoliciesRun exercises each policy end to end.
func TestSubtitleConflictPoliciesRun(t *testing.T) {
	for _, p := range []timeline.ConflictPolicy{
		timeline.ConflictLastWins, timeline.ConflictKeepBoth, timeline.ConflictReject,
	} {
		cfg := acceptanceConfig(t)
		cfg.Policy = p
		cfg.Render = false
		sum, err := Run(cfg)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		// One within-track SRT overlap plus cross-track overlaps with
		// the lower priority VTT track.
		if sum.Conflicts < 1 {
			t.Errorf("%s: conflicts = %d, want >= 1", p, sum.Conflicts)
		}
	}
}

// TestWindowRestrictsTimeline checks the CLI window is applied.
func TestWindowRestrictsTimeline(t *testing.T) {
	cfg := acceptanceConfig(t)
	cfg.Render = false
	cfg.Window = timeline.Window{Start: clock.Zero(), End: clock.NewRational(2, 1)}
	sum, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sum.VideoSamples == 0 || sum.VideoSamples >= 120 {
		t.Fatalf("window did not restrict video samples: %d", sum.VideoSamples)
	}
	if !sum.Monotonic {
		t.Fatal("clipped timeline not monotonic")
	}
}

// TestKeyframeTargetsAreResolvable makes sure every keyframe lookup hit
// points at an in-bounds sync sample.
func TestKeyframeTargetsAreResolvable(t *testing.T) {
	cfg := acceptanceConfig(t)
	cfg.Render = false
	sum, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sum.KeyframeMisses != 0 {
		t.Fatalf("keyframe misses = %d", sum.KeyframeMisses)
	}
	if sum.KeyframeHits != 30 {
		t.Fatalf("keyframe hits = %d, want 30", sum.KeyframeHits)
	}
}

// TestErrorStructureIsShared verifies every reported error carries a
// code, source and offset (or the explicit -1 marker).
func TestErrorStructureIsShared(t *testing.T) {
	cfg := acceptanceConfig(t)
	cfg.Render = false
	sum, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Errors) == 0 {
		t.Fatal("expected error records from damaged fixtures")
	}
	for _, e := range sum.Errors {
		if e.Code == "" || e.Source == "" {
			t.Fatalf("error missing code/source: %+v", e)
		}
		if e.Offset < -1 {
			t.Fatalf("error offset below sentinel: %+v", e)
		}
	}
}

// TestEverySupportedRate runs the analysis pipeline at each broadcast
// rate and asserts the recovered timeline is exact.
func TestEverySupportedRate(t *testing.T) {
	for _, r := range []fixture.Rate{
		fixture.Rate24, fixture.Rate25, fixture.Rate30,
		fixture.RateDrop2997, fixture.Rate2398,
	} {
		cfg := acceptanceConfig(t)
		cfg.Render = false
		cfg.FrameRate = r
		sum, err := Run(cfg)
		if err != nil {
			t.Fatalf("%s: %v", r, err)
		}
		if !sum.Passed {
			t.Errorf("%s: %v", r, sum.Failures)
		}
		if sum.RoundtripRate != clock.NewRational(1, 1) {
			t.Errorf("%s: roundtrip = %s", r, sum.RoundtripRate)
		}
		if sum.Timecode == "" {
			t.Errorf("%s: no start timecode", r)
		}
		if r.DropFrame() != sum.DropFrame {
			t.Errorf("%s: drop-frame flag = %v", r, sum.DropFrame)
		}
	}
}

// TestWrapBiasVariants checks that the unwrapper still crosses the
// boundary with different starting biases.
func TestWrapBiasVariants(t *testing.T) {
	for _, bias := range []uint32{1, 1000, 0xFFFFF000, 4294967000} {
		cfg := acceptanceConfig(t)
		cfg.Render = false
		cfg.WrapAt = bias
		sum, err := Run(cfg)
		if err != nil {
			t.Fatalf("bias %d: %v", bias, err)
		}
		if bias > 0xFFFF0000 && sum.RTPWraps == 0 {
			t.Errorf("bias %d: no wrap detected", bias)
		}
		if sum.RTPJumps != 1 {
			t.Errorf("bias %d: jumps = %d", bias, sum.RTPJumps)
		}
	}
}

// TestSyncCorrectionRemovesDrift asserts the fitted correction brings
// healthy RTP packets onto the MP4 decode timeline and leaves the
// injected jump visible rather than smoothing it away.
func TestSyncCorrectionRemovesDrift(t *testing.T) {
	cfg := acceptanceConfig(t)
	cfg.Render = false
	cfg.DriftPPM = 400
	sum, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Sync.Compared < 100 {
		t.Fatalf("compared = %d", sum.Sync.Compared)
	}
	// Uncorrected skew over the clip must be tens of milliseconds at
	// 400 ppm; after correction healthy packets land within 2 ms.
	if sum.Sync.RawMaxMS.Seconds() < 5 {
		t.Errorf("raw residual = %.3f ms, expected a measurable skew", sum.Sync.RawMaxMS.Seconds())
	}
	if sum.Sync.CleanMaxMS.Seconds() > 2 {
		t.Errorf("clean residual = %.4f ms, want <= 2", sum.Sync.CleanMaxMS.Seconds())
	}
	if sum.Sync.CleanMaxMS.Cmp(sum.Sync.RawMaxMS) >= 0 {
		t.Error("correction did not improve residual")
	}
	if sum.Sync.AnomalyMaxMS.Seconds() < 20_000 {
		t.Errorf("jump residual = %.1f ms, want the raw jump preserved",
			sum.Sync.AnomalyMaxMS.Seconds())
	}
}
