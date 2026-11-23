package txn

import (
	"os"
	"path/filepath"
	"testing"

	"mediarun/internal/clock"
)

func openTestManager(t *testing.T, dir string) *Manager {
	t.Helper()
	m, err := NewManager(filepath.Join(dir, "wal.log"), filepath.Join(dir, "art"),
		"run-1")
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func TestCommitWritesFinalAtomically(t *testing.T) {
	dir := t.TempDir()
	m := openTestManager(t, dir)
	final := filepath.Join(dir, "out", "stage1.json")
	err := m.Execute(Stage{
		Name: "parse", Cursor: "manifest:0", Input: []byte("input"),
		Output: []byte("output-bytes"), Final: final,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("final missing: %v", err)
	}
	if string(got) != "output-bytes" {
		t.Fatalf("final = %q", got)
	}
	// The verified stage artefact is retained as the resume point, and no
	// partial file may survive.
	art, err := os.ReadFile(filepath.Join(dir, "art", "parse.tmp"))
	if err != nil || string(art) != "output-bytes" {
		t.Fatalf("stage artefact not retained: %q err=%v", art, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "art", "parse.tmp.part")); !os.IsNotExist(err) {
		t.Fatalf("partial artefact left behind: %v", err)
	}
	if m.Commits != 1 || m.Rollbacks != 0 {
		t.Fatalf("commits=%d rollbacks=%d", m.Commits, m.Rollbacks)
	}
}

func TestDiskFullLeavesNoFinal(t *testing.T) {
	dir := t.TempDir()
	m := openTestManager(t, dir)
	m.FailOnSync = true
	final := filepath.Join(dir, "out.json")
	err := m.Execute(Stage{Name: "stage", Output: []byte("x"), Final: final})
	if err == nil {
		t.Fatalf("expected disk-full failure")
	}
	if !clock.IsKind(err, clock.KindRecovery) {
		t.Fatalf("wrong kind: %v", err)
	}
	if _, statErr := os.Stat(final); !os.IsNotExist(statErr) {
		t.Fatalf("final artefact exists despite failure")
	}
	if m.Rollbacks != 1 {
		t.Fatalf("rollbacks = %d", m.Rollbacks)
	}
	if _, ok := m.Checkpointed("stage"); ok {
		t.Fatalf("failed stage left a checkpoint")
	}
}

func TestInterruptedCommitRollsBack(t *testing.T) {
	dir := t.TempDir()
	m := openTestManager(t, dir)
	m.FailOnCommit = true
	final := filepath.Join(dir, "out.json")
	err := m.Execute(Stage{Name: "stage", Output: []byte("y"), Final: final})
	if err == nil {
		t.Fatalf("expected interruption")
	}
	if _, statErr := os.Stat(final); !os.IsNotExist(statErr) {
		t.Fatalf("final artefact exists after interruption")
	}
}

func TestReplayDiscardsTruncatedTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.log")
	l, err := OpenLog(path, "run")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		r := Record{Type: RecCheckpoint, Stage: "s"}
		if err := l.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()
	// Simulate a crash mid-append: append half a JSON record.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"stage-commit","seq":6`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	recs, err := ReplayFile(path)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(recs) != 5 {
		t.Fatalf("replay kept %d records, want 5", len(recs))
	}
	for _, r := range recs {
		if !r.Verify() {
			t.Fatalf("record failed verification: %+v", r)
		}
	}
}

func TestReplayDiscardsTamperedRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.log")
	l, _ := OpenLog(path, "run")
	for i := 0; i < 3; i++ {
		l.Append(Record{Type: RecCheckpoint, Stage: "s", OutputHash: "abc"})
	}
	l.Close()
	data, _ := os.ReadFile(path)
	// Flip a byte inside the second record only, so the first stays valid.
	firstEnd := 0
	for i, c := range data {
		if c == '\n' {
			firstEnd = i + 1
			break
		}
	}
	if firstEnd == 0 {
		t.Fatalf("no record terminator")
	}
	if len(data) <= firstEnd {
		t.Fatalf("log too short")
	}
	data[firstEnd] ^= 0x01
	os.WriteFile(path, data, 0o600)
	recs, err := ReplayFile(path)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("tampered log kept %d records, want 1 (tail discarded)", len(recs))
	}
}

func TestSimulatedDiskFullDoesNotWriteFinal(t *testing.T) {
	dir := t.TempDir()
	m := openTestManager(t, dir)
	m.log.FailAfter = 2 // begin + checkpoint succeed, commit append fails
	final := filepath.Join(dir, "out.json")
	err := m.Execute(Stage{Name: "s", Output: []byte("z"), Final: final})
	if err == nil {
		t.Fatalf("expected failure on commit append")
	}
	// The rename already happened, but the commit record did not; the run is
	// therefore not durable. Removing the final artefact keeps invariants.
	if _, statErr := os.Stat(final); statErr == nil {
		t.Fatalf("final artefact present without a durable commit record")
	}
}

func TestCheckpointSummaryStableAcrossResume(t *testing.T) {
	dir := t.TempDir()
	m := openTestManager(t, dir)
	stages := []Stage{
		{Name: "a", Input: []byte("i1"), Output: []byte("o1")},
		{Name: "b", Input: []byte("o1"), Output: []byte("o2")},
	}
	for _, s := range stages {
		if err := m.Execute(s); err != nil {
			t.Fatalf("execute %s: %v", s.Name, err)
		}
	}
	sum1 := m.CheckpointSummary()
	m.Close()

	m2 := openTestManager(t, dir)
	recs, err := ReplayFile(filepath.Join(dir, "wal.log"))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	m2.Resume(recs)
	if _, ok := m2.Checkpointed("a"); !ok {
		t.Fatalf("stage a not resumed")
	}
	if _, ok := m2.Checkpointed("b"); !ok {
		t.Fatalf("stage b not resumed")
	}
	if m2.CheckpointSummary() != sum1 {
		t.Fatalf("summary changed across resume")
	}
}

func TestResumeSkipsCorruptArtefact(t *testing.T) {
	dir := t.TempDir()
	m := openTestManager(t, dir)
	if err := m.Execute(Stage{Name: "a", Output: []byte("o1")}); err != nil {
		t.Fatal(err)
	}
	m.Close()
	// Corrupt the artefact on disk.
	os.WriteFile(filepath.Join(dir, "art", "a.tmp"), []byte("tampered"), 0o600)
	m2 := openTestManager(t, dir)
	recs, _ := ReplayFile(filepath.Join(dir, "wal.log"))
	m2.Resume(recs)
	if _, ok := m2.Checkpointed("a"); ok {
		t.Fatalf("corrupt artefact was resumed")
	}
}

func TestRollbackToCheckpointRemovesNewer(t *testing.T) {
	dir := t.TempDir()
	m := openTestManager(t, dir)
	finals := map[string]string{
		"a": filepath.Join(dir, "a.json"),
		"b": filepath.Join(dir, "b.json"),
		"c": filepath.Join(dir, "c.json"),
	}
	for _, n := range []string{"a", "b", "c"} {
		if err := m.Execute(Stage{Name: n, Output: []byte(n), Final: finals[n]}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.RollbackToCheckpoint("a", finals); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, ok := m.Checkpointed("b"); ok {
		t.Fatalf("b survived rollback")
	}
	if _, err := os.Stat(finals["b"]); !os.IsNotExist(err) {
		t.Fatalf("b final survived rollback")
	}
	if _, ok := m.Checkpointed("a"); !ok {
		t.Fatalf("a removed by rollback")
	}
}
