package txn

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"archivecheck/internal/fault"
	"archivecheck/internal/journal"
)

func newManager(t *testing.T, root string) (*Manager, map[Stage]Artifact) {
	t.Helper()
	fs := &fault.Set{}
	m, done, err := New(Config{Dir: filepath.Join(root, "state"), OutDir: filepath.Join(root, "out"), Faults: fs})
	if err != nil {
		t.Fatal(err)
	}
	return m, done
}

func writeContent(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

func TestStageCommitsAtomically(t *testing.T) {
	root := t.TempDir()
	m, done := newManager(t, root)
	if len(done) != 0 {
		t.Fatalf("fresh manager has %d committed", len(done))
	}
	var ran int
	a, err := m.Stage(StageParse, nil, nil, func(tmp string) error {
		ran++
		return writeContent(tmp, "parsed-data")
	})
	if err != nil {
		t.Fatal(err)
	}
	if ran != 1 || a.Digest == "" {
		t.Fatalf("ran=%d artifact=%+v", ran, a)
	}
	if _, err := os.Stat(a.Path); err != nil {
		t.Fatalf("artifact not visible: %v", err)
	}
	// Re-running a committed stage is a no-op.
	if _, err := m.Stage(StageParse, nil, nil, func(tmp string) error {
		ran++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if ran != 1 {
		t.Fatalf("committed stage re-ran: %d", ran)
	}
	m.Close()

	// Reopen: the committed stage is adopted.
	m2, done2 := newManager(t, root)
	defer m2.Close()
	if len(done2) != 1 || done2[StageParse].Digest != a.Digest {
		t.Fatalf("resume state: %+v", done2)
	}
}

func TestValidationFailureRollsBack(t *testing.T) {
	root := t.TempDir()
	m, _ := newManager(t, root)
	defer m.Close()
	fs := &fault.Set{}
	_, err := m.Stage(StageIndex, nil, func(a Artifact) error {
		return errors.New("digest mismatch downstream")
	}, func(tmp string) error {
		return writeContent(tmp, "index")
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
	if m.Rollbacks() != 1 {
		t.Fatalf("rollbacks=%d", m.Rollbacks())
	}
	if len(m.Committed()) != 0 {
		t.Fatal("failed stage must not be committed")
	}
	// The artifact must not be visible at its final path.
	if _, err := os.Stat(filepath.Join(root, "out", "index.artifact")); !os.IsNotExist(err) {
		t.Fatalf("partial artifact visible: %v", err)
	}
	_ = fs
}

func TestInterruptedAfterPrepareRollsBackOnRestart(t *testing.T) {
	root := t.TempDir()
	m, _ := newManager(t, root)
	// Simulate an interruption between prepare and commit/closing by directly
	// writing a prepare record and a final artifact.
	prep := []byte(`{"stage":"repair","path":"` + filepath.Join(root, "out", "repair.artifact") + `"}`)
	if _, err := m.wal.Append(journal.Record{Stage: "repair", Op: journal.OpPrepare, Payload: prep, Digest: "d1"}); err != nil {
		t.Fatal(err)
	}
	_ = writeContent(filepath.Join(root, "out", "repair.artifact"), "half-baked")
	m.Close()

	m2, done := newManager(t, root)
	defer m2.Close()
	if len(done) != 0 {
		t.Fatalf("uncommitted stage adopted: %+v", done)
	}
	if m2.Rollbacks() == 0 {
		t.Fatal("prepared-without-commit should roll back")
	}
}

func TestCommittedArtifactCorruptionRollsBack(t *testing.T) {
	root := t.TempDir()
	m, _ := newManager(t, root)
	a, err := m.Stage(StageManifest, nil, nil, func(tmp string) error {
		return writeContent(tmp, "manifest-v1")
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	// Corrupt the artifact after commit.
	_ = writeContent(a.Path, "tampered")

	m2, done := newManager(t, root)
	defer m2.Close()
	if _, ok := done[StageManifest]; ok {
		t.Fatal("corrupt artifact must not be adopted")
	}
	if m2.Rollbacks() == 0 {
		t.Fatal("corruption should trigger rollback")
	}
}

func TestProducerFailureRollsBack(t *testing.T) {
	root := t.TempDir()
	m, _ := newManager(t, root)
	defer m.Close()
	_, err := m.Stage(StageRepair, nil, nil, func(tmp string) error {
		return errors.New("boom")
	})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err=%v", err)
	}
	if m.Rollbacks() != 1 || len(m.Committed()) != 0 {
		t.Fatalf("rollbacks=%d committed=%v", m.Rollbacks(), m.Committed())
	}
}

func TestResumeOnlyReRunsIncompleteStages(t *testing.T) {
	root := t.TempDir()
	m, _ := newManager(t, root)
	for _, s := range []Stage{StageParse, StageRepair} {
		s := s
		if _, err := m.Stage(s, nil, nil, func(tmp string) error {
			return writeContent(tmp, string(s))
		}); err != nil {
			t.Fatal(err)
		}
	}
	m.Close()

	ran := map[Stage]int{}
	m2, done := newManager(t, root)
	defer m2.Close()
	for _, s := range Order {
		s := s
		_, err := m2.Stage(s, nil, nil, func(tmp string) error {
			ran[s]++
			return writeContent(tmp, string(s))
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if ran[StageParse] != 0 || ran[StageRepair] != 0 {
		t.Fatalf("redid committed stages: %+v", ran)
	}
	if ran[StageIndex] != 1 || ran[StageManifest] != 1 {
		t.Fatalf("did not run pending stages: %+v", ran)
	}
	if len(done) != 2 {
		t.Fatalf("resume done=%d", len(done))
	}
}

func TestTornJournalTailTolerated(t *testing.T) {
	root := t.TempDir()
	m, _ := newManager(t, root)
	if _, err := m.Stage(StageParse, nil, nil, func(tmp string) error {
		return writeContent(tmp, "ok")
	}); err != nil {
		t.Fatal(err)
	}
	walPath := m.WALPath()
	// Append a partial record to simulate an interrupt mid-append, leaving
	// every completed commit intact.
	f, err := os.OpenFile(walPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte{0x40, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	f.Write([]byte(`{"seq":9,"stage":"repair"`))
	f.Close()
	m.Close()
	m2, done := newManager(t, root)
	defer m2.Close()
	if len(done) != 1 {
		t.Fatalf("committed stage lost after torn tail: %d", len(done))
	}
}
