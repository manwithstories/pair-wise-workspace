package txn

import (
	"os"
	"testing"

	"mediad/internal/model"
)

func TestCommitAndResume(t *testing.T) {
	dir := t.TempDir()
	m, err := Open(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Commit(StageProbe, []byte(`{"tracks":2}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Commit(StageRepair, []byte(`{"repairs":1}`)); err != nil {
		t.Fatal(err)
	}
	m.Close()

	m2, err := Open(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	resume, err := m2.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if resume != StageRTP {
		t.Fatalf("resume=%q want rtp (first uncommitted stage)", resume)
	}
	done := m2.Committed()
	if len(done) != 2 {
		t.Fatalf("committed=%v", done)
	}
	if done[StageProbe].SHA256 == "" {
		t.Fatal("checkpoint missing digest")
	}
}

func TestInterruptMidStageLeavesNoCommit(t *testing.T) {
	dir := t.TempDir()
	m, _ := Open(dir, 0)
	if _, err := m.Commit(StageProbe, []byte(`ok`)); err != nil {
		t.Fatal(err)
	}
	// Begin + data without commit = interrupted stage.
	m.wal.Append(KindBegin, string(StageIndex), nil)
	m.wal.Append(KindData, string(StageIndex), []byte(`partial`))
	m.Close()

	m2, _ := Open(dir, 0)
	defer m2.Close()
	done := m2.Committed()
	if _, ok := done[StageIndex]; ok {
		t.Fatal("interrupted stage was treated as committed")
	}
	resume, err := m2.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if resume != StageRTP {
		t.Fatalf("resume=%q want rtp", resume)
	}
	if _, err := os.Stat(m2.ArtifactPath(StageIndex)); !os.IsNotExist(err) {
		t.Fatal("partial artifact should not exist")
	}
}

func TestStageSizeLimit(t *testing.T) {
	dir := t.TempDir()
	m, _ := Open(dir, 16)
	defer m.Close()
	_, err := m.Commit(StageProbe, make([]byte, 17))
	if err == nil {
		t.Fatal("expected over-limit error")
	}
	if model.KindOf(err) != model.KindOverLimit {
		t.Fatalf("kind=%v", model.KindOf(err))
	}
}

func TestCheckpointCarriesDigest(t *testing.T) {
	dir := t.TempDir()
	m, _ := Open(dir, 0)
	defer m.Close()
	cp, err := m.Commit(StageIndex, []byte(`{"entries":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if cp.SHA256 == "" || cp.Bytes != 13 {
		t.Fatalf("checkpoint=%+v", cp)
	}
	if cp.Artifact != "03-index.json" {
		t.Fatalf("artifact=%q", cp.Artifact)
	}
}
