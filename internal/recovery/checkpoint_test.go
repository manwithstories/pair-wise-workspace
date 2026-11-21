package recovery

import (
	"crypto/sha256"
	"path/filepath"
	"testing"

	"mkvarchive/internal/model"
)

func TestCheckpointMarkAndResume(t *testing.T) {
	dir := t.TempDir()
	sum := sha256.Sum256([]byte("input-bytes"))
	store := NewStore(dir, "in.mkv", 11, sum[:])

	if got := store.LastCompletedStage(); got != "" {
		t.Fatalf("fresh store must have no stage, got %q", got)
	}
	if err := store.Mark(StageCheckpoint{Stage: StageParsed, BlocksSeen: 10}); err != nil {
		t.Fatalf("mark parsed: %v", err)
	}
	if err := store.Mark(StageCheckpoint{Stage: StagePlanned, BlocksSeen: 10,
		PlanSHA256: HashPlan([]byte("plan"))}); err != nil {
		t.Fatalf("mark planned: %v", err)
	}
	// Reopen from disk: the record must survive a new process/store instance.
	reopened := NewStore(dir, "in.mkv", 11, sum[:])
	if got := reopened.LastCompletedStage(); got != StagePlanned {
		t.Fatalf("want last=%s, got %q", StagePlanned, got)
	}
	next, err := ResumeFrom(reopened.LastCompletedStage())
	if err != nil || next != StageWritten {
		t.Fatalf("resume target wrong: %q %v", next, err)
	}
	if cp, ok := reopened.Stage(StageParsed); !ok || cp.BlocksSeen != 10 {
		t.Fatalf("parsed checkpoint not retained: %+v", cp)
	}
}

func TestCheckpointDetectsInputChange(t *testing.T) {
	dir := t.TempDir()
	sum1 := sha256.Sum256([]byte("input-bytes"))
	store := NewStore(dir, "in.mkv", 11, sum1[:])
	if err := store.Mark(StageCheckpoint{Stage: StageParsed, BlocksSeen: 1}); err != nil {
		t.Fatal(err)
	}
	sum2 := sha256.Sum256([]byte("different-input"))
	err := store.VerifyInput(11, sum2[:])
	ae, ok := err.(*model.ArchiveError)
	if !ok || ae.Code != "CHECKPOINT_INPUT_MISMATCH" {
		t.Fatalf("want input mismatch, got %v", err)
	}
}

func TestCheckpointBadStageRejected(t *testing.T) {
	dir := t.TempDir()
	sum := sha256.Sum256([]byte("x"))
	store := NewStore(dir, "in.mkv", 1, sum[:])
	if err := store.Mark(StageCheckpoint{Stage: "moon-shot"}); err == nil {
		t.Fatal("unknown stage must not be persisted")
	}
	if _, err := ResumeFrom("moon-shot"); err == nil {
		t.Fatal("resume from unknown stage must fail")
	}
}

func TestCheckpointFilePersisted(t *testing.T) {
	dir := t.TempDir()
	sum := sha256.Sum256([]byte("x"))
	store := NewStore(dir, "in.mkv", 1, sum[:])
	if err := store.Mark(StageCheckpoint{Stage: StageVerified, PayloadCount: 42}); err != nil {
		t.Fatal(err)
	}
	cp, err := store.load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cp.SchemaVersion != CheckpointSchemaVersion ||
		cp.Stages[StageVerified].PayloadCount != 42 {
		t.Fatalf("persisted record wrong: %+v", cp)
	}
	if next, err := ResumeFrom(StageVerified); err == nil || next != "" {
		t.Fatalf("complete pipeline must not resume: next=%q err=%v", next, err)
	}
	_ = filepath.Join(dir, CheckpointName)
}
