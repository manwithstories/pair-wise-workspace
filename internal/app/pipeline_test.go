package app

import (
	"os"
	"path/filepath"
	"testing"

	"archiver/internal/archive"
)

func runOK(t *testing.T, cfg Config) *Metrics {
	t.Helper()
	m, err := Run(cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return m
}

func TestCleanRunMetrics(t *testing.T) {
	m := runOK(t, Config{Case: "clean", Seed: 73, Out: t.TempDir()})
	if m.RoundTrip != 100 || m.KeyframeHit != 100 || m.SegmentHashMatch != 100 {
		t.Fatalf("metrics: %+v", m)
	}
	if m.Segments < 5 {
		t.Fatalf("segments = %d, want >= 5", m.Segments)
	}
	if m.SampleRate != 48000 || m.Layout != "stereo" {
		t.Fatalf("audio spec: %d %s", m.SampleRate, m.Layout)
	}
	if m.IntegratedLUFS < -18.5 || m.IntegratedLUFS > -17.5 {
		t.Fatalf("lufs = %.2f", m.IntegratedLUFS)
	}
	if m.TruePeakDBTP > -1.0 {
		t.Fatalf("true peak = %.2f", m.TruePeakDBTP)
	}
}

func TestDamagedRunMetrics(t *testing.T) {
	m := runOK(t, Config{
		Case: "damaged", Seed: 73, Out: t.TempDir(),
		Inject: []string{"truncated-extension", "duplicate", "wrap", "late-vfr"},
	})
	if m.FirstErrorOffset < 0 {
		t.Fatal("first error offset not reported")
	}
	if m.Duplicates == 0 || m.Replays == 0 || m.Quarantined == 0 || m.Lost == 0 {
		t.Fatalf("damage counters: %+v", m)
	}
	if m.SegmentHashMatch != 100 {
		t.Fatalf("segment hash match = %.2f", m.SegmentHashMatch)
	}
}

func TestDeterministicHashes(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()
	runOK(t, Config{Case: "clean", Seed: 73, Out: d1})
	runOK(t, Config{Case: "clean", Seed: 73, Out: d2})
	b1, _ := os.ReadFile(filepath.Join(d1, "manifest.json"))
	b2, _ := os.ReadFile(filepath.Join(d2, "manifest.json"))
	if string(b1) != string(b2) {
		t.Fatal("same input, params and seed must yield identical manifests")
	}
}

func TestInterruptAndResume(t *testing.T) {
	dir := t.TempDir()
	m := runOK(t, Config{Case: "recovery", Seed: 73, Out: dir, InterruptAfter: 3})
	if !m.Interrupted || m.Segments != 3 {
		t.Fatalf("interrupt: %+v", m)
	}
	if _, err := archive.LoadCheckpoint(dir); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	m = runOK(t, Config{Out: dir, Resume: true})
	if m.ReusedSegments != 3 || m.FreshHashMatch != 100 {
		t.Fatalf("resume: %+v", m)
	}
	if m.SegmentHashMatch != 100 || m.Segments < 5 {
		t.Fatalf("final: %+v", m)
	}
}

func TestResumeRefusedOnTamperedCheckpoint(t *testing.T) {
	dir := t.TempDir()
	runOK(t, Config{Case: "recovery", Seed: 73, Out: dir, InterruptAfter: 3})
	// Corrupt the recorded input digest.
	p := filepath.Join(dir, "checkpoint.json")
	b, _ := os.ReadFile(p)
	tampered := []byte(string(b)[:20] + "X" + string(b)[21:])
	if err := os.WriteFile(p, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Run(Config{Out: dir, Resume: true})
	if err == nil {
		t.Fatal("tampered checkpoint must refuse resume")
	}
	ie, ok := err.(*InputError)
	if !ok {
		t.Fatalf("error type = %T, want InputError", err)
	}
	t.Logf("refusal reason: %s", ie.Reason)
}

func TestResumeWithoutCheckpoint(t *testing.T) {
	_, err := Run(Config{Out: t.TempDir(), Resume: true})
	if err == nil {
		t.Fatal("resume without checkpoint must fail")
	}
}
