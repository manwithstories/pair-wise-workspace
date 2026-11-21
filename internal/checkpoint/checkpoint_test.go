package checkpoint

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func params1() Params {
	return Params{Seed: 41, LossPercent: 3, ReorderPercent: 7, WrapAt: 4294967000,
		DriftPPM: 180, VFR: true, CheckpointEvery: 256}
}

func TestOpenWriteAndResume(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, 4, params1())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.WriteShard(i, bytes.NewReader([]byte("payload-"+string(rune('a'+i))))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Advance(30, 2, 4); err != nil {
		t.Fatal(err)
	}
	got, st, err := Resume(dir, 4, params1())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Shards) != 3 || st.Cursor != 30 || st.TimeBaseVersion != 2 {
		t.Fatalf("resume state: %+v", st)
	}
	if got.State().ParamDigest != params1().Digest() {
		t.Fatal("param digest mismatch")
	}
}

func TestParamsChangedRejected(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, 4, params1())
	_ = s.Finalize(1, 1)
	other := params1()
	other.Seed = 99
	_, _, err := Resume(dir, 4, other)
	if !errors.Is(err, ErrParamsChanged) {
		t.Fatalf("want ErrParamsChanged, got %v", err)
	}
}

func TestTornCheckpoint(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, 4, params1())
	for i := 0; i < 3; i++ {
		if _, err := s.WriteShard(i, bytes.NewReader([]byte(strings.Repeat("x", 10+i)))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Finalize(12, 1); err != nil {
		t.Fatal(err)
	}
	// Simulate a torn tail: truncate the last shard after the state file
	// recorded its full hash. Resume must reuse only the intact prefix.
	last := filepath.Join(dir, "shard-000002.bin")
	if err := os.Truncate(last, 3); err != nil {
		t.Fatal(err)
	}
	_, st, err := Resume(dir, 4, params1())
	if err != nil {
		t.Fatalf("torn tail should trim, not fatal: %v", err)
	}
	if len(st.Shards) != 2 || st.Shards[1].Index != 1 {
		t.Fatalf("complete shards only: %+v", st.Shards)
	}
}

func TestCorruptStateRejected(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, 4, params1())
	_ = s.Finalize(1, 1)
	if err := os.WriteFile(filepath.Join(dir, stateName), []byte("{not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Resume(dir, 4, params1()); !errors.Is(err, ErrCheckpointCorrupt) {
		t.Fatalf("want ErrCheckpointCorrupt, got %v", err)
	}
}

func TestCursorStopsOnWriteFailure(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, 1, params1())
	// Remove write permission on the directory so the atomic rename fails.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	err := s.Advance(99, 1, 1)
	if err == nil {
		t.Fatal("persist failure must surface as an error")
	}
	if s.State().Cursor != 0 {
		t.Fatalf("cursor must not advance on failure: %d", s.State().Cursor)
	}
}

func TestDeterministicParamDigest(t *testing.T) {
	if params1().Digest() != params1().Digest() {
		t.Fatal("param digest must be stable")
	}
	empty := Params{}
	if params1().Digest() == empty.Digest() {
		t.Fatal("different params must hash differently")
	}
}
