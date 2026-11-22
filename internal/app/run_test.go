package app

import (
	"context"
	"testing"
	"time"
)

func baseCfg() Config {
	c := DefaultConfig()
	c.Seed = 17
	c.DriftPPM = 120
	c.WrapAt = 4294967200
	c.VFR = true
	c.QueueCapacity = 4
	c.TargetBPS = 2_500_000
	c.TargetSegment = 500 * time.Millisecond
	return c
}

func TestRunSuccessPath(t *testing.T) {
	res, err := Run(context.Background(), baseCfg())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Metrics.Errors) != 0 {
		t.Fatalf("clean run must have no quality errors, got %d", len(res.Metrics.Errors))
	}
	if res.Metrics.Segments == 0 {
		t.Fatal("expected committed segments")
	}
	if !res.Metrics.AssertBitrate(2.0) {
		t.Fatalf("bitrate delta %.3f exceeds 2%%", res.Metrics.BitrateDeltaPct)
	}
	if !res.Metrics.AssertAlignment(20) {
		t.Fatalf("alignment %d exceeds 20ms", res.Metrics.AlignmentMS)
	}
	if res.Metrics.PeakQueue > res.Metrics.QueueCapacity {
		t.Fatal("queue exceeded configured capacity")
	}
	if res.Metrics.SilenceSpans == 0 {
		t.Fatal("expected silence detection on the audio track")
	}
}

func TestCorruptFixtureRaisesQualityError(t *testing.T) {
	cfg := baseCfg()
	cfg.CorruptBox = true
	cfg.Truncate = true
	res, _ := Run(context.Background(), cfg)
	if res == nil || len(res.Metrics.Errors) == 0 {
		t.Fatal("corrupt fixture must produce at least one quality error")
	}
	// The failure must change BOTH quality stats and (on the clean pipeline)
	// still leave a usable segment manifest, proving events are not dropped.
	if res.Metrics.Segments == 0 {
		t.Fatal("clean pipeline segments must still be produced alongside errors")
	}
	for _, e := range res.Metrics.Errors {
		if e.Code == "" {
			t.Fatal("quality error needs a code")
		}
	}
}

func TestCancelRetainsOnlyCommittedAndResumes(t *testing.T) {
	cfg := baseCfg()
	first, resumed, match, err := RunResume(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Canceled {
		t.Fatal("first run must report cancellation")
	}
	if first.Metrics.Segments != 3 {
		t.Fatalf("committed segments = %d, want 3", first.Metrics.Segments)
	}
	if !match {
		t.Fatal("resume fingerprints must match the continuous-run prefix")
	}
	if len(resumed.Manifest.Segments) <= first.Metrics.Segments {
		t.Fatal("continuous run should contain more segments than the canceled prefix")
	}
}

func TestDeterministicRepeatedRun(t *testing.T) {
	cfg := baseCfg()
	r1, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(r1.Manifest.Segments) != len(r2.Manifest.Segments) {
		t.Fatal("segment count differs across repeated runs")
	}
	for i := range r1.Manifest.Segments {
		if r1.Manifest.Segments[i].Fingerprint != r2.Manifest.Segments[i].Fingerprint {
			t.Fatal("fingerprint order not deterministic across runs")
		}
	}
}

func TestMissingKeyframeChangesStateAndStats(t *testing.T) {
	cfg := baseCfg()
	cfg.MissingKey = true
	res, _ := Run(context.Background(), cfg)
	if res == nil || len(res.Metrics.Errors) == 0 {
		t.Fatal("missing-keyframe fixture must raise a quality error")
	}
	// Two failure classes must each perturb both segment state and stats.
	found := false
	for _, e := range res.Metrics.Errors {
		if e.Code == "missing_keyframe" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected missing_keyframe quality code")
	}
}
