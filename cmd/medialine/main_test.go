package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runDemoCtx(t *testing.T, cfg demoConfig) (*demoResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return demo(ctx, cfg)
}

func baseConfig(t *testing.T) demoConfig {
	return demoConfig{
		Seed: 17, Tracks: 4, Bitrates: 3, SegmentUS: 2_000_000,
		Queue: 4, Workers: 3, Corrupt: "none", Out: t.TempDir(), DurationUS: 12_000_000,
	}
}

func TestDemoMeetsAcceptanceMetrics(t *testing.T) {
	cfg := baseConfig(t)
	res, err := runDemoCtx(t, cfg)
	if err != nil {
		t.Fatalf("demo: %v", err)
	}
	if res.RoundTripRate < 0.98 || res.RoundTripRate > 1.0 {
		t.Fatalf("roundtrip_rate=%v outside [0.98,1.00]", res.RoundTripRate)
	}
	if res.KeyframeHitRate < 0.95 || res.KeyframeHitRate > 1.0 {
		t.Fatalf("keyframe_hit_rate=%v outside [0.95,1.00]", res.KeyframeHitRate)
	}
	if res.SegmentMatchNum != res.SegmentMatchDen {
		t.Fatalf("hash match %d/%d: denominator must be the planned total", res.SegmentMatchNum, res.SegmentMatchDen)
	}
	if res.SegmentMatchDen != res.Total {
		t.Fatalf("denominator %d != planned total %d", res.SegmentMatchDen, res.Total)
	}
	if res.PeakInflight > res.InflightLimit {
		t.Fatalf("peak %d exceeds limit %d", res.PeakInflight, res.InflightLimit)
	}
	if res.AlignedBoundary != 0 {
		t.Fatalf("aligned_boundary_us=%d", res.AlignedBoundary)
	}
	if res.Completed != res.Total {
		t.Fatalf("completed %d != total %d", res.Completed, res.Total)
	}
	if res.Damaged || res.Aborted {
		t.Fatalf("clean run flagged: %+v", res)
	}
}

func TestDemoResumeAfterCancelDoesNotDoubleCount(t *testing.T) {
	cfg := baseConfig(t)
	cfg.CancelAfter = 5
	res, err := runDemoCtx(t, cfg)
	if err != nil {
		t.Fatalf("demo: %v", err)
	}
	if !res.Aborted {
		t.Fatal("expected the first pass to be aborted")
	}
	if res.Completed != res.Total {
		t.Fatalf("resume should finish every segment: %d/%d", res.Completed, res.Total)
	}
	if res.RecoveredSegments != res.Completed {
		t.Fatalf("recovered=%d completed=%d: resume double-counted", res.RecoveredSegments, res.Completed)
	}
	if res.PeakInflight > res.InflightLimit {
		t.Fatalf("peak %d exceeds limit %d", res.PeakInflight, res.InflightLimit)
	}
}

func TestDemoCorruptTailIsRecoverable(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Corrupt = "tail"
	res, err := runDemoCtx(t, cfg)
	if err != nil {
		t.Fatalf("demo: %v", err)
	}
	if !res.Damaged {
		t.Fatal("tail damage must be reported")
	}
	if res.Completed == 0 {
		t.Fatal("usable fragments must still be produced")
	}
	if res.SegmentMatchNum != res.Completed {
		t.Fatalf("all completed fragments must verify: %d vs %d", res.SegmentMatchNum, res.Completed)
	}
	if res.SegmentMatchDen != res.Total {
		t.Fatalf("denominator %d != planned total %d", res.SegmentMatchDen, res.Total)
	}
}

func TestDemoDeterministic(t *testing.T) {
	cfgA := baseConfig(t)
	resA, err := runDemoCtx(t, cfgA)
	if err != nil {
		t.Fatal(err)
	}
	cfgB := baseConfig(t)
	cfgB.Seed = cfgA.Seed
	resB, err := runDemoCtx(t, cfgB)
	if err != nil {
		t.Fatal(err)
	}
	a := readManifestBytes(t, cfgA.Out)
	b := readManifestBytes(t, cfgB.Out)
	if strings.ReplaceAll(string(a), cfgA.Out, "OUT") != strings.ReplaceAll(string(b), cfgB.Out, "OUT") {
		t.Fatal("manifests differ between identical runs")
	}
	if resA.Completed != resB.Completed || resA.SegmentMatchNum != resB.SegmentMatchNum {
		t.Fatal("segment counts differ between identical runs")
	}
}

func readManifestBytes(t *testing.T, dir string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestExitCodes(t *testing.T) {
	if code := run([]string{"bogus"}); code != exitFatal {
		t.Fatalf("unknown subcommand exit=%d", code)
	}
	if code := run([]string{"demo", "--out"}); code != exitFatal {
		t.Fatalf("missing flag value exit=%d", code)
	}
	if code := run([]string{"demo", "--out", t.TempDir(), "--workers", "0"}); code != exitFatal {
		t.Fatalf("bad workers exit=%d", code)
	}
	if code := run([]string{"demo", "--out", t.TempDir(), "--corrupt", "bogus"}); code != exitFatal {
		t.Fatalf("bad corrupt mode exit=%d", code)
	}
	dir := t.TempDir()
	if code := run([]string{"verify", "--out", dir}); code != exitFatal {
		t.Fatalf("verify without manifest exit=%d", code)
	}
}

func TestDemoEndToEndExitCodes(t *testing.T) {
	clean := t.TempDir()
	if code := run([]string{"demo", "--out", clean, "--tracks", "2", "--bitrates", "2", "--duration-us", "4000000"}); code != exitOK {
		t.Fatalf("clean demo exit=%d", code)
	}
	if code := run([]string{"verify", "--out", clean}); code != exitOK {
		t.Fatalf("verify exit=%d", code)
	}
	damaged := t.TempDir()
	code := run([]string{"demo", "--out", damaged, "--tracks", "2", "--bitrates", "2", "--duration-us", "4000000", "--corrupt", "tail"})
	if code != exitPartial {
		t.Fatalf("damaged demo exit=%d want %d", code, exitPartial)
	}
	// The delivered fragments must still verify cleanly.
	if code := run([]string{"verify", "--out", damaged}); code != exitOK {
		t.Fatalf("verify damaged delivery exit=%d", code)
	}
}

func TestFlagValidation(t *testing.T) {
	bad := []demoConfig{
		{Seed: -1, Tracks: 1, Bitrates: 1, SegmentUS: 1_000_000, Workers: 1, Out: "x", DurationUS: 2_000_000},
		{Tracks: 0, Bitrates: 1, SegmentUS: 1_000_000, Workers: 1, Out: "x", DurationUS: 2_000_000},
		{Tracks: 1, Bitrates: 0, SegmentUS: 1_000_000, Workers: 1, Out: "x", DurationUS: 2_000_000},
		{Tracks: 1, Bitrates: 1, SegmentUS: 1, Workers: 1, Out: "x", DurationUS: 2_000_000},
		{Tracks: 1, Bitrates: 1, SegmentUS: 1_000_000, Workers: 1, Out: "x", DurationUS: 1},
		{Tracks: 1, Bitrates: 1, SegmentUS: 1_000_000, Queue: -1, Workers: 1, Out: "x", DurationUS: 2_000_000},
		{Tracks: 1, Bitrates: 1, SegmentUS: 1_000_000, Workers: 1, DurationUS: 2_000_000},
	}
	for i, cfg := range bad {
		if err := cfg.validate(); err == nil {
			t.Errorf("config %d should be rejected: %+v", i, cfg)
		}
	}
}
