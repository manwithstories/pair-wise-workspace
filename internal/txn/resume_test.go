package txn

import (
	"os"
	"path/filepath"
	"testing"
)

func TestArtifactTamperForcesRollback(t *testing.T) {
	dir := t.TempDir()
	m, _ := Open(dir, 0)
	m.Commit(StageProbe, []byte(`probe`))
	m.Commit(StageRTP, []byte(`rtp`))
	m.Commit(StageRepair, []byte(`repair`))
	m.Close()

	path := filepath.Join(dir, "01-rtp.json")
	os.WriteFile(path, []byte(`tampered`), 0o644)

	m2, _ := Open(dir, 0)
	defer m2.Close()
	resume, err := m2.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if resume != StageRTP {
		t.Fatalf("resume=%q want rtp after rollback", resume)
	}
	if m2.RollbackStage() != string(StageProbe) {
		t.Fatalf("rollback stage=%q want probe", m2.RollbackStage())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("corrupt artifact should have been removed")
	}
	done := m2.Committed()
	if _, ok := done[StageProbe]; !ok {
		t.Fatal("probe should remain committed")
	}
	if _, ok := done[StageRepair]; ok {
		t.Fatal("repair should have been rolled back")
	}
}

func TestCorruptTailTruncatedOnRecover(t *testing.T) {
	dir := t.TempDir()
	m, _ := Open(dir, 0)
	m.Commit(StageProbe, []byte(`probe`))
	m.Close()

	path := filepath.Join(dir, "wal.log")
	valid, _ := os.ReadFile(path)
	torn := append(append([]byte(nil), valid...), []byte{0, 0, 0, 40, 1, 2, 3}...)
	os.WriteFile(path, torn, 0o644)

	m2, _ := Open(dir, 0)
	defer m2.Close()
	defer func() {
		st, _ := os.Stat(path)
		if st.Size() != int64(len(valid)) {
			t.Fatalf("log size %d after recovery, want %d", st.Size(), len(valid))
		}
	}()
	resume, err := m2.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if dirty, _ := m2.CorruptTail(); !dirty {
		t.Fatal("corrupt tail not reported")
	}
	if resume != StageRTP {
		t.Fatalf("resume=%q", resume)
	}
}
