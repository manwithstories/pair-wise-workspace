package pipeline

import (
	"testing"

	"mediad/internal/fixture"
	"mediad/internal/model"
	"mediad/internal/txn"
)

func TestSecondProcessResumes(t *testing.T) {
	dir := t.TempDir()
	first := DemoOptions{Seed: 17, Dir: dir, Faults: []fixture.FaultKind{fixture.FaultBurstLoss},
		InterruptAfter: txn.StageRepair}
	if _, err := Run(first); err == nil {
		t.Fatal("first process should report an interruption")
	} else if model.KindOf(err) != model.KindRecovery {
		t.Fatalf("kind=%v", model.KindOf(err))
	}
	// The second process knows nothing except the directory.
	second := DemoOptions{Seed: 17, Dir: dir, Faults: []fixture.FaultKind{fixture.FaultBurstLoss}, Resume: true}
	r, err := Run(second)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	want := []string{"probe", "rtp", "repair"}
	if len(r.ReusedStages) != len(want) {
		t.Fatalf("reused=%v want %v", r.ReusedStages, want)
	}
	if r.Summary.RollbackStage == "" || r.Summary.ResumeStage == "" {
		t.Fatalf("stages missing: %+v", r.Summary)
	}
	if r.Summary.ManifestAlignmentMS > 1 || r.Summary.IndexSeekMismatches != 0 {
		t.Fatalf("acceptance failed after resume: %+v", r.Summary)
	}
	if r.Summary.RecoveredPackets == 0 {
		t.Fatal("recovered packet count was lost across the resume")
	}
}

func TestCorruptLogTriggersRollback(t *testing.T) {
	opt := DemoOptions{Seed: 17, Dir: t.TempDir(), Faults: []fixture.FaultKind{fixture.FaultCorruptLog}}
	r, err := Run(opt)
	if err != nil {
		t.Fatal(err)
	}
	if r.Summary.RollbackStage != string(txn.StageRepair) {
		t.Fatalf("rollback_stage=%q want repair", r.Summary.RollbackStage)
	}
	if r.Summary.ResumeStage != string(txn.StageIndex) {
		t.Fatalf("resume_stage=%q want index", r.Summary.ResumeStage)
	}
	if r.Summary.InjectedFaults == 0 {
		t.Fatal("corrupt-log fault was not counted")
	}
	if r.Summary.IndexSeekMismatches != 0 {
		t.Fatalf("mismatches=%d after rebuild", r.Summary.IndexSeekMismatches)
	}
}

func TestRepeatedResumeIsStable(t *testing.T) {
	dir := t.TempDir()
	opt := DemoOptions{Seed: 17, Dir: dir, Faults: []fixture.FaultKind{fixture.FaultBurstLoss}}
	first, err := Run(opt)
	if err != nil {
		t.Fatal(err)
	}
	opt.Resume = true
	second, err := Run(opt)
	if err != nil {
		t.Fatal(err)
	}
	if first.Summary.ManifestSHA256 != second.Summary.ManifestSHA256 {
		t.Fatal("digest changed across resume")
	}
	if len(second.ReusedStages) != len(txn.Order) {
		t.Fatalf("reused=%v want all stages", second.ReusedStages)
	}
}
