package pipeline

import (
	"os"
	"path/filepath"
	"testing"
)

func baseConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		Seed: 42, Segments: 4, SegmentDurNS: 2_000_000_000,
		PacketRate: 50, WorkDir: t.TempDir(),
		Faults: Faults{Reorder: true, Truncate: true, Drift: true},
	}
}

func TestRunProducesAcceptanceInvariants(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Cameras = []string{"cam0", "cam1", "cam2"}
	res, err := Run(cfg)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.RoundtripRate < MinRoundtripRate {
		t.Fatalf("roundtrip = %v", res.RoundtripRate)
	}
	if res.KeyframeHitRate < MinKeyframeHitRate {
		t.Fatalf("keyframe hit = %v", res.KeyframeHitRate)
	}
	if res.SegmentHashMatch != 1.0 {
		t.Fatalf("segment hash match = %v", res.SegmentHashMatch)
	}
	if !res.ProvenanceComplete {
		t.Fatalf("provenance incomplete: %+v", res)
	}
	if res.RecoveredGaps == 0 {
		t.Fatalf("seed sample must exercise gap recovery")
	}
	if res.Segments != cfg.Segments*len(cfg.Cameras) {
		t.Fatalf("segments = %d", res.Segments)
	}
	// The final manifest must exist and be the only durable final artefact.
	if _, err := os.Stat(filepath.Join(cfg.WorkDir, "final.json")); err != nil {
		t.Fatalf("final manifest missing: %v", err)
	}
}

func TestDiskFullLeavesNoFinalManifest(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Inject = InjectDiskFull
	if _, err := Run(cfg); err == nil {
		t.Fatalf("disk-full run reported success")
	}
	if _, err := os.Stat(filepath.Join(cfg.WorkDir, "final.json")); err != nil {
		if !os.IsNotExist(err) {
			t.Fatalf("stat final: %v", err)
		}
	} else {
		t.Fatalf("half-written final manifest survived a disk-full failure")
	}
}

func TestInterruptedCommitLeavesNoFinalManifest(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Inject = InjectCommitCrash
	cfg.InjectStage = "jitter"
	if _, err := Run(cfg); err == nil {
		t.Fatalf("interrupted run reported success")
	}
	if _, err := os.Stat(filepath.Join(cfg.WorkDir, "final.json")); !os.IsNotExist(err) {
		t.Fatalf("final manifest survived an interrupted commit")
	}
}

func TestResumeMatchesCleanRerun(t *testing.T) {
	clean := baseConfig(t)
	clean.WorkDir = filepath.Join(t.TempDir(), "clean")
	cleanRes, err := Run(clean)
	if err != nil {
		t.Fatalf("clean run: %v", err)
	}

	cfg := baseConfig(t)
	cfg.WorkDir = filepath.Join(t.TempDir(), "resumed")
	cfg.Inject = InjectCommitCrash
	cfg.InjectStage = "sync"
	if _, err := Run(cfg); err == nil {
		t.Fatalf("interrupted run reported success")
	}
	cfg.Inject = InjectNone
	cfg.InjectStage = ""
	cfg.Resume = true
	cfg.ReferenceHash = cleanRes.NormalHash
	res, err := Run(cfg)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !res.ResumeHashEqual {
		t.Fatalf("resumed hash %s != reference %s", res.ResumedHash, cleanRes.NormalHash)
	}
	if res.RollbackCount == 0 {
		t.Fatalf("expected the interruption to be recorded as a rollback")
	}
	if res.RecoveredGaps != cleanRes.RecoveredGaps {
		t.Fatalf("resume changed recovery counts: %d vs %d",
			res.RecoveredGaps, cleanRes.RecoveredGaps)
	}
}

func TestResumeDetectsChangedReference(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Resume = true
	cfg.ReferenceHash = "deadbeef"
	res, err := Run(cfg)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ResumeHashEqual {
		t.Fatalf("resume claimed equality against a bogus reference")
	}
}

func TestOutputIsDeterministic(t *testing.T) {
	a := baseConfig(t)
	a.WorkDir = filepath.Join(t.TempDir(), "a")
	ra, err := Run(a)
	if err != nil {
		t.Fatal(err)
	}
	b := baseConfig(t)
	b.WorkDir = filepath.Join(t.TempDir(), "b")
	rb, err := Run(b)
	if err != nil {
		t.Fatal(err)
	}
	if ra.NormalHash != rb.NormalHash {
		t.Fatalf("work directory leaked into output: %s vs %s",
			ra.NormalHash, rb.NormalHash)
	}
	if ra.TraceSummary != rb.TraceSummary {
		t.Fatalf("trace summary differed between identical runs")
	}
}

func TestMalformedInputRejected(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Segments = 0
	if _, err := Run(cfg); err == nil {
		t.Fatalf("zero-segment sample accepted")
	}
}
