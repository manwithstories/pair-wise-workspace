package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"avcore/internal/audit"
	"avcore/internal/caption"
	"avcore/internal/clock"
	"avcore/internal/demo"
	"avcore/internal/state"
)

func testConfig(t *testing.T, jobDir string, interrupt int) (Config, *demo.Paths) {
	t.Helper()
	srcDir := t.TempDir()
	paths, err := demo.Generate(demo.Spec{
		Dir: srcDir, Seed: 41, DurationS: 4, SegmentS: 1, Rate: clock.Rate30,
	})
	if err != nil {
		if strings.Contains(err.Error(), "ffmpeg not found") {
			t.Skip("ffmpeg unavailable")
		}
		t.Fatal(err)
	}
	cfg := Config{
		Source: paths.Source, JobDir: jobDir, SegmentTicks: clock.TicksPerSecond,
		FrameRate: clock.Rate30, InterruptAfterCuts: interrupt,
		Renditions: []RenditionConfig{
			{Name: "low", TargetBPS: 450000, Width: 320, Height: 240},
			{Name: "high", TargetBPS: 900000, Width: 640, Height: 480},
		},
	}
	return cfg, paths
}

func parseCaps(t *testing.T, path string) caption.Result {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	return caption.ParseSRT(f, caption.MaxCaptionBytes)
}

// newLogger opens an audit JSONL file at path, creating its parent directory.
func newLogger(t *testing.T, path string) *audit.Logger {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := audit.NewLogger(path)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestEndToEndRun(t *testing.T) {
	job := t.TempDir()
	cfg, paths := testConfig(t, job, 0)
	eng, err := NewEngine()
	if err != nil {
		t.Skipf("engine unavailable: %v", err)
	}
	logger := newLogger(t, filepath.Join(t.TempDir(), "e2e.jsonl"))
	defer logger.Close()
	caps := parseCaps(t, paths.SRT)
	res, err := eng.Run(context.Background(), cfg, caps, logger)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Summary.Variants) != 2 {
		t.Fatalf("variants = %d", len(res.Summary.Variants))
	}
	for _, vm := range res.Summary.Variants {
		if vm.SegmentCount != 4 {
			t.Errorf("%s segments = %d, want 4", vm.Name, vm.SegmentCount)
		}
		if vm.PSNRMeanDB < 28 {
			t.Errorf("%s PSNR %.2f below 28", vm.Name, vm.PSNRMeanDB)
		}
		if vm.SSIMMean < 0.90 {
			t.Errorf("%s SSIM %.4f below 0.90", vm.Name, vm.SSIMMean)
		}
		// Allow the documented 15% tolerance.
		if abs(vm.BitrateDeltaPct) > 15 {
			t.Errorf("%s bitrate delta %.2f%% exceeds 15", vm.Name, vm.BitrateDeltaPct)
		}
	}
	if res.Summary.MaxAlignmentMS > 33.4 {
		t.Errorf("alignment %.3f ms exceeds one frame", res.Summary.MaxAlignmentMS)
	}
	// Manifest files exist and reference committed segments.
	for _, f := range []string{"master.m3u8", "v0/index.m3u8", "v1/index.m3u8", "subs/index.m3u8"} {
		if _, err := os.Stat(filepath.Join(job, f)); err != nil {
			t.Errorf("manifest missing %s: %v", f, err)
		}
	}
}

func TestInterruptAndRecovery(t *testing.T) {
	job := t.TempDir()
	eng, err := NewEngine()
	if err != nil {
		t.Skipf("engine unavailable: %v", err)
	}

	// First run: inject an interruption after two cuts are committed.
	cfg1, paths := testConfig(t, job, 2)
	logger1 := newLogger(t, filepath.Join(t.TempDir(), "a.jsonl"))
	caps := parseCaps(t, paths.SRT)
	res1, err := eng.Run(context.Background(), cfg1, caps, logger1)
	if err == nil || !res1.Interrupted {
		t.Fatalf("expected interrupt, got res=%+v err=%v", res1, err)
	}
	if got := countCommittedCuts(res1.Journal); got < 2 {
		t.Fatalf("committed cuts before interrupt = %d, want >=2", got)
	}
	logger1.Close()

	// Second run: no injection, resume from the verified checkpoints.
	cfg2 := cfg1
	cfg2.InterruptAfterCuts = 0
	logger2 := newLogger(t, filepath.Join(t.TempDir(), "b.jsonl"))
	defer logger2.Close()
	res2, err := eng.Run(context.Background(), cfg2, caps, logger2)
	if err != nil {
		t.Fatalf("recovery run: %v", err)
	}
	if res2.Summary.ReusedSegments < 4 { // two cuts reused across two variants
		t.Errorf("reused segments = %d, want >=4", res2.Summary.ReusedSegments)
	}
	if res2.Summary.ManifestSHA256 == "" {
		t.Fatal("recovery produced no manifest hash")
	}

	// A clean full rerun in a fresh job must yield the same manifest digest.
	fresh := t.TempDir()
	cfg3 := cfg2
	cfg3.JobDir = fresh
	logger3 := newLogger(t, filepath.Join(t.TempDir(), "c.jsonl"))
	defer logger3.Close()
	res3, err := eng.Run(context.Background(), cfg3, caps, logger3)
	if err != nil {
		t.Fatalf("full rerun: %v", err)
	}
	if res3.ManifestSHA != res2.ManifestSHA {
		t.Errorf("manifest digest drift after recovery: %s vs %s",
			res3.ManifestSHA, res2.ManifestSHA)
	}
}

func TestCorruptSegmentIsReencoded(t *testing.T) {
	job := t.TempDir()
	eng, err := NewEngine()
	if err != nil {
		t.Skipf("engine unavailable: %v", err)
	}
	cfg, paths := testConfig(t, job, 0)
	logger := newLogger(t, filepath.Join(t.TempDir(), "d.jsonl"))
	defer logger.Close()
	caps := parseCaps(t, paths.SRT)

	// Prime a partial run then tamper with one committed segment.
	prime := cfg
	prime.InterruptAfterCuts = 2
	if _, err := eng.Run(context.Background(), prime, caps, logger); err == nil {
		t.Fatal("expected injected interrupt on prime run")
	}
	target := filepath.Join(job, "v0_000.m4s")
	if err := os.WriteFile(target, []byte("corruption"), 0o644); err != nil {
		t.Fatal(err)
	}
	recover := cfg
	recover.InterruptAfterCuts = 0
	res, err := eng.Run(context.Background(), recover, caps, logger)
	if err != nil {
		t.Fatalf("recovery after corruption: %v", err)
	}
	// The tampered checkpoint was dropped and re-encoded; final run completes.
	if len(res.Journal.Segments) != 8 { // 4 cuts x 2 variants
		t.Errorf("segments after recovery = %d, want 8", len(res.Journal.Segments))
	}
	hash, _ := state.FileSHA256(target)
	if hash == state.HashBytes([]byte("corruption")) {
		t.Error("tampered segment was not replaced")
	}
}

func TestRejectsLessThanTwoRenditions(t *testing.T) {
	eng, _ := NewEngine()
	if eng == nil {
		t.Skip("engine unavailable")
	}
	cfg := Config{
		Source: "x", JobDir: t.TempDir(), SegmentTicks: clock.TicksPerSecond,
		FrameRate: clock.Rate30,
		Renditions: []RenditionConfig{{Name: "only", TargetBPS: 1, Width: 2, Height: 2}},
	}
	_, err := eng.Run(context.Background(), cfg, caption.Result{}, nil)
	if err == nil {
		t.Error("single rendition must be rejected")
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
