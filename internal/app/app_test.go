package app

import (
	"errors"
	"testing"
	"time"

	"bcast/internal/fault"
	"bcast/internal/verify"
)

func TestRunDemoEndToEnd(t *testing.T) {
	opts := Options{
		WorkDir: t.TempDir(), Seed: 41,
		Duration: 90 * time.Second, FaultsAll: true,
		ResumeCutPct: 61, ReviewStep: 15 * time.Second,
	}
	rep, err := RunDemo(opts)
	if err != nil {
		t.Fatalf("RunDemo: %v", err)
	}
	// 验收字段断言。
	if rep.Tracks != 5 {
		t.Fatalf("tracks = %d, want 5", rep.Tracks)
	}
	if rep.Renditions != 3 {
		t.Fatalf("renditions = %d, want 3", rep.Renditions)
	}
	if rep.Frames != 6 { // 90s / 15s
		t.Fatalf("frames = %d, want 6", rep.Frames)
	}
	if rep.BoundaryMismatches != 0 || rep.CrossLayerDeviations != 0 {
		t.Fatalf("boundary=%d cross=%d must be 0", rep.BoundaryMismatches, rep.CrossLayerDeviations)
	}
	if !rep.PackagesIdentical || !rep.BlockVerificationOK || !rep.FrameVerificationOK {
		t.Fatalf("verification flags must all be true: %+v", rep)
	}
	if len(rep.PixelSHA256) != 64 || rep.FullSHA256 != rep.ResumedSHA256 {
		t.Fatalf("hashes invalid: pixel=%d full=%s resumed=%s",
			len(rep.PixelSHA256), rep.FullSHA256, rep.ResumedSHA256)
	}
	// 漂移应精确恢复注入值。
	wantPPM := map[int64]bool{120: true, -80: true, 40: true, 60: true, -45: true}
	if len(rep.DriftPPM) != 5 {
		t.Fatalf("drift ppm = %v", rep.DriftPPM)
	}
	for _, p := range rep.DriftPPM {
		if !wantPPM[p] {
			t.Fatalf("unexpected drift ppm %d in %v", p, rep.DriftPPM)
		}
	}
	// Evaluate 必须通过（退出码 0 语义）。
	if err := verify.Evaluate(*rep); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
}

func TestRunDemoDeterministicAcrossRuns(t *testing.T) {
	common := func(work string) *verify.Report {
		r, err := RunDemo(Options{
			WorkDir: work, Seed: 7, Duration: 60 * time.Second, FaultsAll: true,
			ResumeCutPct: 40, ReviewStep: 15 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r1 := common(t.TempDir())
	r2 := common(t.TempDir())
	if r1.PixelSHA256 != r2.PixelSHA256 || r1.FullSHA256 != r2.FullSHA256 {
		t.Fatalf("nondeterministic: pixel %s/%s full %s/%s",
			r1.PixelSHA256, r2.PixelSHA256, r1.FullSHA256, r2.FullSHA256)
	}
}

func TestRunDemoRejectsInvalidOptions(t *testing.T) {
	bad := Options{WorkDir: "", Duration: time.Second, ReviewStep: time.Second, ResumeCutPct: 50}
	_, err := RunDemo(bad)
	if !errors.Is(err, fault.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput, got %v", err)
	}
	badPct := Options{WorkDir: t.TempDir(), Duration: time.Second,
		ReviewStep: time.Second, ResumeCutPct: 150}
	if _, err := RunDemo(badPct); !errors.Is(err, fault.ErrInvalidInput) {
		t.Fatalf("bad percent want ErrInvalidInput, got %v", err)
	}
}

func TestOptionParsers(t *testing.T) {
	if _, err := ParseDuration("30m"); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseDuration("bad"); !errors.Is(err, fault.ErrInvalidInput) {
		t.Fatalf("bad duration must be ErrInvalidInput")
	}
	pct, err := ParsePercent("61%")
	if err != nil || pct != 61 {
		t.Fatalf("percent = %d, %v", pct, err)
	}
	if _, err := ParsePercent("61"); !errors.Is(err, fault.ErrInvalidInput) {
		t.Fatal("missing %% must be invalid")
	}
	all, err := ParseFaults("all")
	if err != nil || !all {
		t.Fatal("all faults should parse")
	}
	if none, err := ParseFaults("none"); err != nil || none {
		t.Fatal("none should parse to false")
	}
	if _, err := ParseFaults("weird"); !errors.Is(err, fault.ErrInvalidInput) {
		t.Fatal("weird faults must be invalid")
	}
}
