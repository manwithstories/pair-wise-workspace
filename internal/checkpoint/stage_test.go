package checkpoint

import (
	"os"
	"testing"

	"relayaudit/internal/fault"
)

func TestStageChunkAppendsAndTruncates(t *testing.T) {
	s := newStore(t)
	if err := s.StageChunk(0, []byte("hello")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.StageChunk(5, []byte(" world")); err != nil {
		t.Fatalf("extend: %v", err)
	}
	body, err := os.ReadFile(s.StagingPath())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != "hello world" {
		t.Fatalf("staging = %q", body)
	}
	// Rewriting at an earlier offset must drop the stale tail.
	if err := s.StageChunk(5, []byte("!")); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	body, _ = os.ReadFile(s.StagingPath())
	if string(body) != "hello!" {
		t.Fatalf("staging after truncate = %q", body)
	}
}

func TestStageChunkRejectsGap(t *testing.T) {
	s := newStore(t)
	if err := s.StageChunk(0, []byte("abc")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.StageChunk(10, []byte("x")); !fault.IsKind(err, fault.Checkpoint) {
		t.Fatalf("gap accepted: %v", err)
	}
	if err := s.StageChunk(-1, []byte("x")); !fault.IsKind(err, fault.Checkpoint) {
		t.Fatalf("negative offset accepted: %v", err)
	}
}

func TestCommitAppendKeepsDigestConsistent(t *testing.T) {
	s := newStore(t)
	first := []byte("first chunk")
	if err := s.Commit(baseState(first), first); err != nil {
		t.Fatalf("commit: %v", err)
	}
	full := append(append([]byte(nil), first...), []byte("second chunk")...)
	st := State{
		SourceSHA256: PrefixDigest([]byte("source")),
		NextIndex:    20,
		Kept:         18,
		Dropped:      2,
		EmittedBytes: int64(len(full)),
		PrefixSHA256: PrefixDigest(full),
	}
	if err := s.CommitAppend(st, full, []byte("second chunk")); err != nil {
		t.Fatalf("append commit: %v", err)
	}
	got, ok, err := s.Load()
	if err != nil || !ok {
		t.Fatalf("load: %v ok=%v", err, ok)
	}
	if got.PrefixSHA256 != PrefixDigest(full) || got.EmittedBytes != int64(len(full)) {
		t.Fatalf("state = %+v", got)
	}
	staged, _ := s.ReadStaging()
	if string(staged) != string(full) {
		t.Fatalf("staging = %q", staged)
	}
	// A later resume must see a prefix whose digest matches the state.
	if err := s.Save(st); err != nil {
		t.Fatalf("resave: %v", err)
	}
}
