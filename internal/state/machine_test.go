package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"turncode/internal/quality"
	"turncode/internal/rate"
)

func TestMachineIllegalTransitionPreservesState(t *testing.T) {
	m := New()
	if err := m.Transition(Validating); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(Encoding); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("got %v", err)
	}
	if m.State() != Validating {
		t.Fatalf("state changed to %s", m.State())
	}
	for _, step := range []State{Analyzing, Encoding, Verifying, Done} {
		if err := m.Transition(step); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Transition(Encoding); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("done should be terminal")
	}
}

func TestStateStoreChecksumAndBackup(t *testing.T) {
	dir := t.TempDir()
	s := NewStateStore(dir)
	if _, err := s.Save(Validating); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(Encoding); err != nil {
		t.Fatal(err)
	}
	rec, recovered, err := s.Load()
	if err != nil || recovered || rec.State != Encoding {
		t.Fatalf("load: rec=%+v recovered=%v err=%v", rec, recovered, err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte(`{"version":1,"state":"done","sequence":9,"crc":"bad"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, recovered, err = s.Load()
	if err != nil || !recovered || rec.State != Validating {
		t.Fatalf("backup load: rec=%+v recovered=%v err=%v", rec, recovered, err)
	}
}

func TestCheckpointRecoveryTruncationAndFallback(t *testing.T) {
	dir := t.TempDir()
	store := NewCheckpointStore(dir)
	acc := quality.NewAccumulator(60)
	cp1 := Checkpoint{CompletedGOPs: 1, NextFrame: 30, BitstreamOffset: 100, QP: 30,
		Buffer: rate.BufferState{CapacityBits: 1200000}, Quality: *acc}
	if err := store.SaveCheckpoint(cp1); err != nil {
		t.Fatal(err)
	}
	cp2 := cp1
	cp2.CompletedGOPs, cp2.NextFrame, cp2.BitstreamOffset = 2, 60, 200
	if err := store.SaveCheckpoint(cp2); err != nil {
		t.Fatal(err)
	}
	rec, err := store.Recover(150, "")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Checkpoint.CompletedGOPs != 1 || !rec.Fallback {
		t.Fatalf("expected fallback to GOP1: %+v", rec)
	}
	rec, err = store.Recover(250, "")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Checkpoint.CompletedGOPs != 2 || !rec.Truncate {
		t.Fatalf("expected truncation at GOP2: %+v", rec)
	}
	if err := os.WriteFile(filepath.Join(dir, checkpointDir, checkpointName(2)), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, err = store.Recover(250, "")
	if err != nil || rec.Checkpoint.CompletedGOPs != 1 {
		t.Fatalf("corrupt latest did not fall back: %+v err=%v", rec, err)
	}
}
