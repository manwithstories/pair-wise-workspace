package checkpoint

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediapipe/internal/media"
)

func TestJournalCommitsOnlyLandedArtifactAndResumes(t *testing.T) {
	dir := t.TempDir()
	jp := filepath.Join(dir, "checkpoint.json")
	j, err := Open(jp, "job", "src", "cfg")
	if err != nil {
		t.Fatal(err)
	}
	product := filepath.Join(dir, "seg000.mp4")
	if err := os.WriteFile(product, []byte("product"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := j.RecordProduct("seg000.mp4", product, 42); err != nil {
		t.Fatal(err)
	}
	if !j.IsComplete("seg000.mp4") || j.Snapshot().NextSeq != 42 {
		t.Fatal("journal state not advanced")
	}
	j2, err := Open(jp, "job", "src", "cfg")
	if err != nil {
		t.Fatal(err)
	}
	if !j2.IsComplete("seg000.mp4") {
		t.Fatal("checkpoint did not survive reopen")
	}
}

func TestMissingProductIsNotCheckpointed(t *testing.T) {
	j, _ := Open(filepath.Join(t.TempDir(), "j.json"), "j", "", "")
	_, err := j.RecordProduct("missing", filepath.Join(t.TempDir(), "nope.mp4"), 1)
	var me *media.Error
	if !errors.As(err, &me) || me.ExitCode() != 4 || me.Code != media.CodeIO {
		t.Fatalf("expected IO exit4, got %v", err)
	}
	if j.IsComplete("missing") {
		t.Fatal("missing product must not be recorded")
	}
}

func TestCorruptJournalIsCheckpointError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "j.json")
	if err := os.WriteFile(p, []byte("{not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(p, "j", "", "")
	var me *media.Error
	if !errors.As(err, &me) || me.Code != media.CodeCheckpoint || me.ExitCode() != 4 {
		t.Fatalf("expected checkpoint exit4, got %v", err)
	}
}

func TestSourceMismatchRefusesResume(t *testing.T) {
	p := filepath.Join(t.TempDir(), "j.json")
	j, _ := Open(p, "j", "src-a", "cfg")
	if err := j.SetNextSequence(3); err != nil {
		t.Fatal(err)
	}
	_, err := Open(p, "j", "src-b", "cfg")
	if err == nil {
		t.Fatal("expected source mismatch to refuse unsafe resume")
	}
}
