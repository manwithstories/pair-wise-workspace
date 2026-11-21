package pipeline

import (
	"os"
	"testing"

	"mediad/internal/fixture"
	"mediad/internal/model"
	"mediad/internal/txn"
)

func baseOpt(t *testing.T) DemoOptions {
	t.Helper()
	return DemoOptions{Seed: 17, Dir: t.TempDir()}
}

func TestDemoIsDeterministic(t *testing.T) {
	opt := baseOpt(t)
	opt.Faults = []fixture.FaultKind{fixture.FaultDuplicate, fixture.FaultReorder, fixture.FaultBurstLoss}
	opt.BandwidthMbps = []float64{8, 2, 12}
	r1, err := Run(opt)
	if err != nil {
		t.Fatal(err)
	}
	opt.Dir = t.TempDir()
	r2, err := Run(opt)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Summary.ManifestSHA256 != r2.Summary.ManifestSHA256 {
		t.Fatalf("digest differs: %s vs %s", r1.Summary.ManifestSHA256, r2.Summary.ManifestSHA256)
	}
	if r1.Summary.ActualBitrateKbps != r2.Summary.ActualBitrateKbps {
		t.Fatalf("bitrate differs")
	}
	if r1.Summary.InjectedFaults != r2.Summary.InjectedFaults {
		t.Fatalf("fault count differs")
	}
}

func TestDemoMeetsAcceptance(t *testing.T) {
	opt := baseOpt(t)
	opt.Faults = []fixture.FaultKind{fixture.FaultDuplicate, fixture.FaultReorder, fixture.FaultBurstLoss}
	opt.BandwidthMbps = []float64{8, 2, 12}
	r, err := Run(opt)
	if err != nil {
		t.Fatal(err)
	}
	s := r.Summary
	if err := r.Manifest.Gate(1.0, 5.0); err != nil {
		t.Fatalf("manifest gate: %v", err)
	}
	if s.ManifestAlignmentMS > 1 {
		t.Fatalf("alignment=%v ms", s.ManifestAlignmentMS)
	}
	if s.IndexSeekMismatches != 0 {
		t.Fatalf("seek mismatches=%d", s.IndexSeekMismatches)
	}
	if s.RecoveredPackets == 0 {
		t.Fatal("expected recovered packets with burst loss")
	}
	if s.UnrecoverablePackets == 0 {
		t.Fatal("the long burst should be reported unrecoverable")
	}
	if s.Duplicates == 0 || s.Reordered == 0 {
		t.Fatalf("duplicate/reorder counters=%d/%d", s.Duplicates, s.Reordered)
	}
	if s.QualityPSNRdB < 30 {
		t.Fatalf("quality=%v dB", s.QualityPSNRdB)
	}
	if s.ManifestSHA256 == "" {
		t.Fatal("missing manifest digest")
	}
}

func TestInterruptWithoutResumeFails(t *testing.T) {
	opt := baseOpt(t)
	opt.InterruptAfter = txn.StageIndex
	_, err := Run(opt)
	if err == nil {
		t.Fatal("expected recovery failure")
	}
	if model.KindOf(err) != model.KindRecovery {
		t.Fatalf("kind=%v want recovery", model.KindOf(err))
	}
}

func TestResumeReusesVerifiedStages(t *testing.T) {
	opt := baseOpt(t)
	opt.Faults = []fixture.FaultKind{fixture.FaultBurstLoss}
	opt.InterruptAfter = txn.StageIndex
	opt.Resume = true
	r, err := Run(opt)
	if err != nil {
		t.Fatal(err)
	}
	if r.RollbackStage != string(txn.StageIndex) {
		t.Fatalf("rollback stage=%q", r.RollbackStage)
	}
	if r.ResumeStage != string(txn.StageManifest) {
		t.Fatalf("resume stage=%q", r.ResumeStage)
	}
	want := []string{"probe", "rtp", "repair", "index"}
	if len(r.ReusedStages) != len(want) {
		t.Fatalf("reused=%v", r.ReusedStages)
	}
	for i, s := range want {
		if r.ReusedStages[i] != s {
			t.Fatalf("reused=%v want %v", r.ReusedStages, want)
		}
	}
	if r.Summary.ManifestSHA256 == "" || r.Summary.InjectedFaults == 0 {
		t.Fatalf("summary incomplete: %+v", r.Summary)
	}
}

func TestFixturesAreRegenerated(t *testing.T) {
	opt := baseOpt(t)
	if _, err := Run(opt); err != nil {
		t.Fatal(err)
	}
	// Corrupt a fixture; the next run must rewrite it rather than trust it.
	if err := os.WriteFile(opt.Dir+"/"+MP4Name, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(opt); err != nil {
		t.Fatalf("run after fixture corruption: %v", err)
	}
}
