package txn

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCrashAfterTwoWALRecordsRecoversOneCommitAndRollsTemp(t *testing.T) {
	dir := t.TempDir()
	crash, err := NewManager(filepath.Join(dir, "state"), 2)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("deterministic clip blob")
	if _, err := crash.Commit("clip-1", "blobs/clip-1.clip", data); !errors.Is(err, ErrCrashInjected) {
		t.Fatalf("Commit err=%v, want ErrCrashInjected", err)
	}
	if err := crash.Close(); err != nil {
		t.Fatal(err)
	}

	orphan := filepath.Join(dir, "state", "tmp", "orphan.tmp")
	if err := os.WriteFile(orphan, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	resume, err := NewManager(filepath.Join(dir, "state"), 0)
	if err != nil {
		t.Fatal(err)
	}
	report, err := resume.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Committed) != 1 {
		t.Fatalf("committed records=%d, want 1", len(report.Committed))
	}
	if len(report.RolledBackTemp) != 1 {
		t.Fatalf("rolled back temp=%+v", report.RolledBackTemp)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan temp still exists: %v", err)
	}
	p, err := resume.ObjectPath("blobs/clip-1.clip")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil || string(got) != string(data) {
		t.Fatalf("recovered object data=%q err=%v", got, err)
	}

	if _, err := resume.Commit("clip-2", "blobs/clip-2.clip", []byte("second")); err != nil {
		t.Fatalf("resumed commit: %v", err)
	}
	if err := resume.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryFailsInvariantWhenCommittedObjectTampered(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	m, err := NewManager(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Commit("tx", "a.clip", []byte("good")); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "objects", "a.clip")
	if err := os.WriteFile(p, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	m2, err := NewManager(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m2.Recover(); !errors.Is(err, ErrInvariant) {
		t.Fatalf("Recover err=%v, want ErrInvariant", err)
	}
}
