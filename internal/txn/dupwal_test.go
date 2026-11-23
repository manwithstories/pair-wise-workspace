package txn

import (
	"path/filepath"
	"testing"
)

// TestResumeDeduplicatesStageOrder guards a defect where replaying a WAL with
// a stage committed twice appended the stage to the resume order twice,
// leaving a stale entry that a later rollback could not remove cleanly.
func TestResumeDeduplicatesStageOrder(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(filepath.Join(dir, "wal.log"), filepath.Join(dir, "art"), "r")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := m.Execute(Stage{Name: "a", Output: []byte("o")}); err != nil {
			t.Fatal(err)
		}
	}
	m.Close()

	recs, err := ReplayFile(filepath.Join(dir, "wal.log"))
	if err != nil {
		t.Fatal(err)
	}
	m2, err := NewManager(filepath.Join(dir, "wal.log"), filepath.Join(dir, "art"), "r2")
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	m2.Resume(recs)
	if len(m2.order) != 1 {
		t.Fatalf("resume order = %v, want one entry", m2.order)
	}
	if s, ok := m2.LastVerifiedStage(); !ok || s != "a" {
		t.Fatalf("last verified = %q ok=%v", s, ok)
	}
}
