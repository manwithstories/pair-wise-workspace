package txn

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/example/mediad/internal/model"
)

func alwaysOK(string) error { return nil }

func TestJournalRoundTrip(t *testing.T) {
	dir := t.TempDir()
	j, entries, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("new journal must be empty")
	}
	if _, err := j.Begin(StageWrite, "seg-1", "h-in"); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Done(StageWrite, "seg-1", "p", "h-out"); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	_, replayed, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := Analyze(replayed)
	if !r.IsDone(StageWrite, "seg-1") {
		t.Fatal("done boundary must survive replay")
	}
	if len(r.Pending) != 0 {
		t.Fatalf("unexpected pending: %+v", r.Pending)
	}
}

func TestPendingStageRequiresRerun(t *testing.T) {
	entries := []Entry{
		{Seq: 1, Stage: StageWrite, SegmentID: "seg-1", State: "begin"},
	}
	r := Analyze(entries)
	if r.IsDone(StageWrite, "seg-1") {
		t.Fatal("begin without done is not complete")
	}
	if len(r.Pending) != 1 {
		t.Fatalf("pending = %+v", r.Pending)
	}
}

func TestCommitHappyPathAndIdempotency(t *testing.T) {
	dir := t.TempDir()
	j, _, _ := OpenJournal(dir)
	defer j.Close()
	c := NewCommitter(dir, j, CrashNone)
	if err := c.Prepare(); err != nil {
		t.Fatal(err)
	}
	res, err := c.Run("seg-1", []byte("payload"), alwaysOK)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(res.OutputPath); err != nil {
		t.Fatalf("committed artifact missing: %v", err)
	}
	// Temp artifact must be gone after rename.
	if _, err := os.Stat(filepath.Join(dir, "tmp", "seg-1.part.tmp")); !os.IsNotExist(err) {
		t.Fatal("temp artifact must be removed on commit")
	}

	// Second run reuses the committed artifact without rewriting.
	res2, err := c.Run("seg-1", []byte("payload"), alwaysOK)
	if err != nil || !res2.Reused || res2.Hash != res.Hash {
		t.Fatalf("idempotent replay failed: %+v err=%v", res2, err)
	}
}

func TestVerificationFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	j, _, _ := OpenJournal(dir)
	defer j.Close()
	c := NewCommitter(dir, j, CrashNone)
	_ = c.Prepare()

	badVerify := func(string) error {
		return model.New(model.KindChecksum, "txn", "verify", "hash mismatch")
	}
	if _, err := c.Run("seg-x", []byte("p"), badVerify); err == nil {
		t.Fatal("verification failure must surface")
	}
	if c.RollbackCount != 1 {
		t.Fatalf("rollback count = %d", c.RollbackCount)
	}
	if _, err := os.Stat(filepath.Join(dir, "tmp", "seg-x.part.tmp")); !os.IsNotExist(err) {
		t.Fatal("rejected artifact must be rolled back to/removed from temp")
	}
}

func TestCrashBeforeCommitRecovers(t *testing.T) {
	dir := t.TempDir()

	// First process: crash after write, before commit.
	j1, _, _ := OpenJournal(dir)
	c1 := NewCommitter(dir, j1, CrashBeforeCommit)
	_ = c1.Prepare()
	_, err := c1.Run("seg-c", []byte("data"), alwaysOK)
	if me, ok := model.AsError(err); !ok || me.Op != "crash" {
		t.Fatalf("expected injected crash, got %v", err)
	}
	_ = j1.Close()

	// Temp artifact remains, commit boundary is absent.
	tmp := filepath.Join(dir, "tmp", "seg-c.part.tmp")
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("temp artifact should survive crash: %v", err)
	}

	// Recovery process: reopen journal, roll back incomplete work.
	j2, entries, _ := OpenJournal(dir)
	defer j2.Close()
	c2 := NewCommitter(dir, j2, CrashNone)
	rerun, err := c2.Recover(Analyze(entries))
	if err != nil {
		t.Fatal(err)
	}
	if len(rerun) != 1 || rerun[0] != "seg-c" {
		t.Fatalf("rerun = %v", rerun)
	}
	if c2.RollbackCount != 1 {
		t.Fatalf("rollback count = %d", c2.RollbackCount)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("uncommitted artifact must be rolled back")
	}

	// Rerun to completion.
	res, err := c2.Run("seg-c", []byte("data"), alwaysOK)
	if err != nil {
		t.Fatalf("rerun must succeed: %v", err)
	}
	if _, err := os.Stat(res.OutputPath); err != nil {
		t.Fatalf("final artifact missing: %v", err)
	}
}

func TestCrashAfterWriteThenRecover(t *testing.T) {
	dir := t.TempDir()
	j1, _, _ := OpenJournal(dir)
	c1 := NewCommitter(dir, j1, CrashAfterWrite)
	_ = c1.Prepare()
	_, _ = c1.Run("seg-w", []byte("x"), alwaysOK)
	_ = j1.Close()

	j2, entries, _ := OpenJournal(dir)
	defer j2.Close()
	c2 := NewCommitter(dir, j2, CrashNone)
	rerun, err := c2.Recover(Analyze(entries))
	if err != nil {
		t.Fatal(err)
	}
	if len(rerun) != 1 {
		t.Fatalf("rerun = %v", rerun)
	}
}

func TestResetJournal(t *testing.T) {
	dir := t.TempDir()
	j, _, _ := OpenJournal(dir)
	_, _ = j.Begin(StageWrite, "s", "h")
	if err := j.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "journal.jsonl")); !os.IsNotExist(err) {
		t.Fatal("journal file must be removed")
	}
}
