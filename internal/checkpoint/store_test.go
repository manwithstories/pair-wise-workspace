package checkpoint

import (
	"os"
	"path/filepath"
	"testing"

	"relayaudit/internal/fault"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s
}

func baseState(prefix []byte) State {
	return State{
		SourceSHA256: PrefixDigest([]byte("source")),
		NextIndex:    10,
		Kept:         8,
		Dropped:      2,
		EmittedBytes: int64(len(prefix)),
		PrefixSHA256: PrefixDigest(prefix),
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	s := newStore(t)
	prefix := []byte("repaired output prefix")
	st := baseState(prefix)
	if err := s.Commit(st, prefix); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, ok, err := s.Load()
	if err != nil || !ok {
		t.Fatalf("load: %v ok=%v", err, ok)
	}
	if got.NextIndex != 10 || got.Kept != 8 || got.Dropped != 2 {
		t.Fatalf("counters changed: %+v", got)
	}
	if got.PrefixSHA256 != st.PrefixSHA256 || got.EmittedBytes != st.EmittedBytes {
		t.Fatalf("prefix metadata changed")
	}
	if !got.Valid() {
		t.Fatalf("loaded state fails checksum")
	}
}

func TestLoadMissingStateIsNotAnError(t *testing.T) {
	s := newStore(t)
	if _, ok, err := s.Load(); ok || err != nil {
		t.Fatalf("missing state: ok=%v err=%v", ok, err)
	}
}

func TestLoadRejectsTamperedState(t *testing.T) {
	s := newStore(t)
	prefix := []byte("prefix")
	if err := s.Commit(baseState(prefix), prefix); err != nil {
		t.Fatalf("commit: %v", err)
	}
	body, err := os.ReadFile(s.StatePath())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Flip a digit in next_index without fixing the digest.
	tampered := []byte(string(body))
	for i := 0; i+12 < len(tampered); i++ {
		if string(tampered[i:i+12]) == `"next_index"` {
			j := i + 12
			for j < len(tampered) && (tampered[j] < '0' || tampered[j] > '9') {
				j++
			}
			tampered[j] = '7'
			break
		}
	}
	if err := os.WriteFile(s.StatePath(), tampered, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := s.Load(); !fault.IsKind(err, fault.Checkpoint) {
		t.Fatalf("tampered state accepted: %v", err)
	}
}

func TestSaveRefusesToRewind(t *testing.T) {
	s := newStore(t)
	p1 := []byte("one")
	if err := s.Commit(baseState(p1), p1); err != nil {
		t.Fatalf("commit: %v", err)
	}
	st := baseState(p1)
	st.NextIndex = 5
	if err := s.Save(st); !fault.IsKind(err, fault.Checkpoint) {
		t.Fatalf("rewind accepted: %v", err)
	}
}

func TestSaveRefusesToRewriteCommittedPrefix(t *testing.T) {
	s := newStore(t)
	p1 := []byte("one")
	if err := s.Commit(baseState(p1), p1); err != nil {
		t.Fatalf("commit: %v", err)
	}
	other := []byte("two")
	st := baseState(other)
	st.NextIndex = 10
	st.Kept = 8
	st.Dropped = 2
	if err := s.Save(st); !fault.IsKind(err, fault.Checkpoint) {
		t.Fatalf("prefix rewrite accepted: %v", err)
	}
}

func TestSaveRefusesToReopenFinishedState(t *testing.T) {
	s := newStore(t)
	p := []byte("final")
	st := baseState(p)
	st.Done = true
	st.OutputSHA256 = PrefixDigest([]byte("final output"))
	if err := s.Commit(st, p); err != nil {
		t.Fatalf("commit: %v", err)
	}
	reopen := st
	reopen.Done = false
	if err := s.Save(reopen); !fault.IsKind(err, fault.Checkpoint) {
		t.Fatalf("reopen accepted: %v", err)
	}
}

func TestCommitRejectsMismatchedPrefix(t *testing.T) {
	s := newStore(t)
	p := []byte("0123456789")
	st := baseState(p)
	if err := s.Commit(st, p[:5]); !fault.IsKind(err, fault.Checkpoint) {
		t.Fatalf("short prefix accepted: %v", err)
	}
	bad := append([]byte(nil), p...)
	bad[0] ^= 0xff
	if err := s.Commit(st, bad); !fault.IsKind(err, fault.Checkpoint) {
		t.Fatalf("wrong prefix accepted: %v", err)
	}
}

func TestSaveRejectsMissingStagedOutput(t *testing.T) {
	s := newStore(t)
	p := []byte("staged")
	st := baseState(p)
	if err := s.Save(st); !fault.IsKind(err, fault.Checkpoint) {
		t.Fatalf("save without staged bytes accepted: %v", err)
	}
}

func TestAtomicSaveLeavesNoTempFiles(t *testing.T) {
	s := newStore(t)
	p := []byte("payload")
	if err := s.Commit(baseState(p), p); err != nil {
		t.Fatalf("commit: %v", err)
	}
	entries, err := os.ReadDir(s.Dir())
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" && e.Name() != "repaired.part" {
			t.Fatalf("unexpected leftover artefact %q", e.Name())
		}
	}
}

func TestWriteOutputAndReset(t *testing.T) {
	s := newStore(t)
	if err := s.WriteOutput([]byte("final bytes")); err != nil {
		t.Fatalf("write output: %v", err)
	}
	got, err := s.ReadOutput()
	if err != nil || string(got) != "final bytes" {
		t.Fatalf("read output: %q err=%v", got, err)
	}
	if err := s.Reset(); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, err := os.Stat(s.OutputPath()); !os.IsNotExist(err) {
		t.Fatalf("output survived reset")
	}
	if _, ok, _ := s.Load(); ok {
		t.Fatalf("state survived reset")
	}
}

func TestHashPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("abcdef"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := HashPrefix(path, 3)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if want := PrefixDigest([]byte("abc")); got != want {
		t.Fatalf("prefix hash = %s, want %s", got, want)
	}
	if _, err := HashPrefix(path, 99); err != nil {
		t.Fatalf("over-long prefix should clamp to EOF: %v", err)
	}
	if _, err := HashPrefix(path, -1); err == nil {
		t.Fatalf("negative prefix accepted")
	}
}

func TestNewStoreRejectsEmptyDir(t *testing.T) {
	if _, err := NewStore(""); err == nil {
		t.Fatalf("empty dir accepted")
	}
}
