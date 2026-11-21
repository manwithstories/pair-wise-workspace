package model

import (
	"errors"
	"fmt"
	"testing"
)

func TestExitCodeMapping(t *testing.T) {
	if got := New(KindInvalidMedia, "probe", "bad stsz").ExitCode(); got != 2 {
		t.Errorf("invalid media exit=%d want 2", got)
	}
	if got := New(KindRecovery, "txn", "torn wal").ExitCode(); got != 3 {
		t.Errorf("recovery exit=%d want 3", got)
	}
	if got := New(KindThreshold, "report", "delta").ExitCode(); got != 4 {
		t.Errorf("threshold exit=%d want 4", got)
	}
	if got := ExitCodeOf(nil); got != 0 {
		t.Errorf("nil exit=%d", got)
	}
}

func TestErrorChain(t *testing.T) {
	cause := errors.New("io")
	e := Wrap(KindRecovery, "txn", "commit", cause).WithTrack(3)
	if KindOf(e) != KindRecovery {
		t.Fatalf("kind=%v", KindOf(e))
	}
	if !errors.Is(e, cause) {
		t.Fatal("chain does not expose cause")
	}
	want := "recovery: txn: track=3: commit: io"
	if e.Error() != want {
		t.Fatalf("Error()=%q want %q", e.Error(), want)
	}
	if KindOf(fmt.Errorf("plain")) != KindInternal {
		t.Fatal("plain error should be internal")
	}
	got, ok := AsError(fmt.Errorf("outer: %w", e))
	if !ok || got.TrackID != 3 {
		t.Fatalf("AsError=%v %v", got, ok)
	}
}
