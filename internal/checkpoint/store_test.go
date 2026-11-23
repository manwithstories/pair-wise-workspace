package checkpoint

import (
	"os"
	"path/filepath"
	"testing"

	"mediacore/internal/fault"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(filepath.Join(dir, "ckpt.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := State{Version: 1, CommittedInputSeq: 42, CommittedSamples: 100,
		PerTrack: map[uint32]int{1: 40, 2: 60}, ResumeTick: 9000, InputDigest: "abc"}
	if err := st.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.CommittedInputSeq != 42 || got.CommittedSamples != 100 ||
		got.ResumeTick != 9000 || got.PerTrack[2] != 60 || got.InputDigest != "abc" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestMissingReturnsZero(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewStore(filepath.Join(dir, "nope.json"))
	got, err := st.Load()
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if got.Version != 1 {
		t.Fatal("want zero/first-run state")
	}
}

func TestCorruptRollsBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ckpt.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, _ := NewStore(path)
	fallback := State{Version: 1, CommittedInputSeq: 7}
	var seen *fault.Fault
	got := st.LoadOrRollback(fallback, func(f *fault.Fault) { seen = f })
	if seen == nil || seen.Strategy != fault.Rollback {
		t.Fatalf("want rollback fault, got %v", seen)
	}
	if got.CommittedInputSeq != 7 {
		t.Fatal("corrupt checkpoint must fall back to last intact boundary")
	}
}

func TestAtomicReplaceLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewStore(filepath.Join(dir, "ckpt.json"))
	if err := st.Save(State{Version: 1, CommittedInputSeq: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ckpt.json.tmp")); !os.IsNotExist(err) {
		t.Fatal("temporary checkpoint file must be removed after atomic rename")
	}
}

func TestRejectsBadPath(t *testing.T) {
	if _, err := NewStore("/"); err == nil {
		t.Fatal("root path must be rejected")
	}
	if _, err := NewStore(".."); err == nil {
		t.Fatal("traversal path must be rejected")
	}
}

func TestNoDoubleCountOnResume(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewStore(filepath.Join(dir, "ckpt.json"))
	// Commit boundary at seq 10 / 50 samples, then reload and ensure the next
	// commit advances from (not restarts at) zero.
	first := State{Version: 1, CommittedInputSeq: 10, CommittedSamples: 50,
		PerTrack: map[uint32]int{1: 50}}
	if err := st.Save(first); err != nil {
		t.Fatal(err)
	}
	loaded, _ := st.Load()
	loaded.CommittedInputSeq = 20
	loaded.CommittedSamples = 90
	loaded.PerTrack[1] = 90
	if err := st.Save(loaded); err != nil {
		t.Fatal(err)
	}
	again, _ := st.Load()
	if again.CommittedSamples != 90 {
		t.Fatalf("resume must continue from committed boundary, got %d", again.CommittedSamples)
	}
}
